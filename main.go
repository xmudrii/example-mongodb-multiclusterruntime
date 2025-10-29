package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	apisv1alpha1 "github.com/kcp-dev/kcp/sdk/apis/apis/v1alpha1"
	corev1alpha1 "github.com/kcp-dev/kcp/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/kcp/sdk/apis/tenancy/v1alpha1"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	mongodbv1 "github.com/mongodb/mongodb-kubernetes-operator/api/v1"
	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	kubezap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	mcsource "sigs.k8s.io/multicluster-runtime/pkg/source"
)

func init() {
	runtime.Must(tenancyv1alpha1.AddToScheme(scheme.Scheme))
	runtime.Must(corev1alpha1.AddToScheme(scheme.Scheme))
	runtime.Must(apisv1alpha1.AddToScheme(scheme.Scheme))
	runtime.Must(mongodbv1.AddToScheme(scheme.Scheme))
}

// label to place on synced objects to indicate which kcp logicalcluster they belong to
const clusterLabel = "example-mongodb-multiclusterruntime/cluster"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "An error occured while running the example:\n%v\n", err)
		os.Exit(1)
	}
}

type reconciler struct {
	log           *logr.Logger
	targetClient  ctrlruntimeclient.Client
	clusterGetter func(context.Context, string) (cluster.Cluster, error)
}

func run() error {
	opts := struct {
		// kubeconfig to connect to kcp provider workspace where APIExport exists
		kcpkubeconfig string
		// kubeconfig of target cluster where objects will be synced to
		targetkubeconfig string
	}{}

	flag.StringVar(&opts.kcpkubeconfig, "kcp-kubeconfig", "", "kubeconfig to connect to kcp provider workspace where APIExport exists")
	flag.StringVar(&opts.targetkubeconfig, "target-kubeconfig", "", "kubeconfig of target cluster where objects will be synced to")

	zapOpts := kubezap.Options{
		Development: true,
	}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	if opts.kcpkubeconfig == "" || opts.targetkubeconfig == "" {
		return errors.New("both --kcp-kubeconfig and --target-kubeconfig must be specified")
	}

	logger := kubezap.New(kubezap.UseFlagOptions(&zapOpts))
	ctrl.SetLogger(logger)
	log := logger.WithName("setup")

	// setup downstream reconciliation
	kcpConfig, err := clientcmd.BuildConfigFromFlags("", opts.kcpkubeconfig)
	if err != nil {
		return err
	}

	provider, err := apiexport.New(kcpConfig, apiexport.Options{})
	if err != nil {
		return err
	}

	mgr, err := mcmanager.New(kcpConfig, provider, manager.Options{
		Logger: logger.WithName("manager"),
	})
	if err != nil {
		return err
	}

	// setup upstream reconciliation
	targetConfig, err := clientcmd.BuildConfigFromFlags("", opts.targetkubeconfig)
	if err != nil {
		return err
	}
	targetClient, err := ctrlruntimeclient.New(targetConfig, ctrlruntimeclient.Options{})
	if err != nil {
		return err
	}

	// build the reconciler
	rec := &reconciler{
		log:           &logger,
		targetClient:  targetClient,
		clusterGetter: mgr.GetCluster,
	}
	if err := rec.SetupWithManager(mgr, targetConfig, log); err != nil {
		return err
	}
	log.Info("Setup complete")

	log.Info("Starting provider and manager")
	g, ctx := errgroup.WithContext(context.Background())
	g.Go(func() error { return wrapError("provider", provider.Run(ctx, mgr)) })
	g.Go(func() error { return wrapError("manager", mgr.Start(ctx)) })

	return g.Wait()
}

func (r *reconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (reconcile.Result, error) {
	clusterName := req.ClusterName
	log := r.log.
		WithName("reconciler").
		WithValues("clustername", clusterName).
		WithValues("object", req.NamespacedName.String())

	log.Info("Starting Reconciliation")

	cluster, err := r.clusterGetter(ctx, clusterName)
	if err != nil {
		log.Error(err, "failed to get cluster")
		return reconcile.Result{}, err
	}
	kcpclient := cluster.GetClient()

	up := &mongodbv1.MongoDBCommunity{}
	upErr := kcpclient.Get(ctx, req.NamespacedName, up)
	if ctrlruntimeclient.IgnoreNotFound(upErr) != nil {
		log.Error(err, "failed to get mongodb from kcp")
		return reconcile.Result{}, err
	}
	upExists := !apierrors.IsNotFound(upErr)

	down := &mongodbv1.MongoDBCommunity{}
	downErr := r.targetClient.Get(ctx, req.NamespacedName, down)
	if ctrlruntimeclient.IgnoreNotFound(downErr) != nil {
		log.Error(err, "failed to get mongodb from downstream cluster")
		return reconcile.Result{}, err
	}
	downExists := !apierrors.IsNotFound(downErr)

	// if upstream does not exist, make sure downstream also does not exist
	if !upExists {
		if !downExists {
			// everything is fine
			return reconcile.Result{}, nil
		}
		log.Info("upstream was deleted, but downstream still exists. Deleting downstream now")
		err = r.targetClient.Delete(ctx, down)
		if err != nil {
			log.Error(err, "failed to delete downstream mongodb")
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	// if upstream exists but downstream does not, create downstream
	if upExists && !downExists {
		log.Info("upstream exists but downstream does not. Creating downstream now")
		n := up.DeepCopy()
		stripMetadata(n)
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[clusterLabel] = clusterName
		err = r.targetClient.Create(ctx, n)
		if err != nil {
			log.Error(err, "failed to create downstream mongodb")
			return reconcile.Result{}, err
		}
		// requeue so we fetch a fresh copy of downstream for comparisons down below
		return reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}

	specDiff := !cmp.Equal(up.Spec, down.Spec)
	// if both exist, sync spec from upstream to downstream
	if specDiff {
		log.Info("specs differ between upstream and downstream. Syncing downstream to match upstream")
		down.Spec = up.Spec
		err = r.targetClient.Update(ctx, down)
		if err != nil {
			log.Error(err, "failed to update downstream mongodb spec")
			return reconcile.Result{}, err
		}
	}

	// if both have the same spec, sync the status back from downstream to upstream
	statusDiff := !cmp.Equal(up.Status, down.Status)
	if statusDiff {
		log.Info("status differs between upstream and downstream. Syncing downstream to upstream")
		up.Status = down.Status
		err = kcpclient.Status().Update(ctx, up)
		if err != nil {
			log.Error(err, "failed to update upstream mongodb status")
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) SetupWithManager(mgr mcmanager.Manager, downstreamCfg *rest.Config, log logr.Logger) error {
	options := mccontroller.Options{
		Reconciler: r,
	}
	c, err := mccontroller.New("mongodb-syncer", mgr, options)
	if err != nil {
		return err
	}

	// watch the mongodbs in kcp (upstream)
	if err := c.MultiClusterWatch(mcsource.TypedKind(&mongodbv1.MongoDBCommunity{}, mchandler.TypedEnqueueRequestForObject[*mongodbv1.MongoDBCommunity]())); err != nil {
		return fmt.Errorf("failed to establish kcp watch: %w", err)
	}

	// watch the mongodbs in downstream clusters, but enqueue the original upstream object
	ca, err := newWrappedCache(downstreamCfg)
	if err != nil {
		return fmt.Errorf("failed to build cache for cluster watch: %w", err)
	}
	err = mgr.Add(ca)
	if err != nil {
		return fmt.Errorf("failed to add cache to manager: %w", err)
	}
	enqueRemoteObj := handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, o *mongodbv1.MongoDBCommunity) []mcreconcile.Request {
		clustername, ok := o.Labels[clusterLabel]
		if !ok {
			log.Info("was not able to find label %q on downstream object %s/%s", clusterLabel, o.Namespace, o.Name)
			return nil
		}
		return []mcreconcile.Request{
			{
				ClusterName: clustername,
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: o.Namespace,
						Name:      o.Name,
					},
				},
			}}
	})
	if err := c.Watch(source.TypedKind(ca, &mongodbv1.MongoDBCommunity{}, enqueRemoteObj)); err != nil {
		return fmt.Errorf("failed to establish downstream cluster watch: %w", err)
	}

	return nil
}

func wrapError(producer string, err error) error {
	return fmt.Errorf("%s: %w", producer, err)
}

func stripMetadata(obj ctrlruntimeclient.Object) {
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetFinalizers(nil)
	obj.SetGeneration(0)
	obj.SetOwnerReferences(nil)
	obj.SetResourceVersion("")
	obj.SetManagedFields(nil)
	obj.SetUID("")
	obj.SetSelfLink("")
}

// wrappedCache is a helper type to make a controller-runtime cache usable with multicluster managers.
// It's Engage is a no-op.
// We need this here so we can directly manage the cache using a manager since no multicluster-runtime cache package exists.
// The alternative would be to have two managers which is more complex and also if the cache crashes the other manager
// whos watches are actually using the cache would not be aware of it.
// It should only be created through newWrappedCache.
type wrappedCache struct {
	cache.Cache
	multicluster.Aware
}

func (wrappedCache) Engage(_ context.Context, _ string, _ cluster.Cluster) (_ error) {
	// no-op, since we just want to wrap and not actually implement a multicluster cache.
	return nil
}

func newWrappedCache(cfg *rest.Config) (*wrappedCache, error) {
	c, err := cache.New(cfg, cache.Options{})
	if err != nil {
		return &wrappedCache{}, err
	}
	return &wrappedCache{
		Cache: c,
	}, nil
}

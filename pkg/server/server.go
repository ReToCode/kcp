/*
Copyright 2021 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"context"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/kcp-dev/logicalcluster/v2"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/genericcontrolplane"

	configroot "github.com/kcp-dev/kcp/config/root"
	configrootphase0 "github.com/kcp-dev/kcp/config/root-phase0"
	configshard "github.com/kcp-dev/kcp/config/shard"
	systemcrds "github.com/kcp-dev/kcp/config/system-crds"
	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	bootstrappolicy "github.com/kcp-dev/kcp/pkg/authorization/bootstrap"
	kcpfeatures "github.com/kcp-dev/kcp/pkg/features"
	"github.com/kcp-dev/kcp/pkg/indexers"
	"github.com/kcp-dev/kcp/pkg/informer"
)

const resyncPeriod = 10 * time.Hour

// systemShardCluster is the name of a logical cluster on every shard (including the root shard) that holds essential system resources (like the root APIs).
var systemShardCluster = logicalcluster.New("system:shard")

type Server struct {
	CompletedConfig

	*genericcontrolplane.ServerChain

	syncedCh chan struct{}
}

func (s *Server) AddPostStartHook(name string, hook genericapiserver.PostStartHookFunc) error {
	return s.MiniAggregator.GenericAPIServer.AddPostStartHook(name, hook)
}

func NewServer(c CompletedConfig) (*Server, error) {
	s := &Server{
		CompletedConfig: c,
		syncedCh:        make(chan struct{}),
	}

	var err error
	s.ServerChain, err = genericcontrolplane.CreateServerChain(c.MiniAggregator, c.Apis, c.ApiExtensions)
	if err != nil {
		return nil, err
	}

	s.GenericControlPlane.GenericAPIServer.Handler.GoRestfulContainer.Filter(
		mergeCRDsIntoCoreGroup(
			s.ApiExtensions.ExtraConfig.ClusterAwareCRDLister,
			s.CustomResourceDefinitions.GenericAPIServer.Handler.NonGoRestfulMux.ServeHTTP,
			s.GenericControlPlane.GenericAPIServer.Handler.GoRestfulContainer.ServeHTTP,
		),
	)

	s.DynamicDiscoverySharedInformerFactory, err = informer.NewDynamicDiscoverySharedInformerFactory(
		s.MiniAggregator.GenericAPIServer.LoopbackClientConfig,
		func(obj interface{}) bool { return true },
		s.ApiExtensionsSharedInformerFactory.Apiextensions().V1().CustomResourceDefinitions(),
		indexers.NamespaceScoped(),
	)
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Server) Run(ctx context.Context) error {
	delegationChainHead := s.MiniAggregator.GenericAPIServer

	if err := s.AddPostStartHook("kcp-bootstrap-policy", bootstrappolicy.Policy().EnsureRBACPolicy()); err != nil {
		return err
	}

	if err := s.AddPostStartHook("kcp-start-informers", func(ctx genericapiserver.PostStartHookContext) error {
		s.KubeSharedInformerFactory.Start(ctx.StopCh)
		s.ApiExtensionsSharedInformerFactory.Start(ctx.StopCh)

		s.KubeSharedInformerFactory.WaitForCacheSync(ctx.StopCh)
		s.ApiExtensionsSharedInformerFactory.WaitForCacheSync(ctx.StopCh)

		select {
		case <-ctx.StopCh:
			return nil // context closed, avoid reporting success below
		default:
		}

		klog.Infof("Finished start kube informers")

		if err := systemcrds.Bootstrap(
			goContext(ctx),
			s.ApiExtensionsClusterClient.Cluster(SystemCRDLogicalCluster),
			s.ApiExtensionsClusterClient.Cluster(SystemCRDLogicalCluster).Discovery(),
			s.DynamicClusterClient.Cluster(SystemCRDLogicalCluster),
		); err != nil {
			klog.Errorf("failed to bootstrap system CRDs: %v", err)
			// nolint:nilerr
			return nil // don't klog.Fatal. This only happens when context is cancelled.
		}
		klog.Infof("Finished bootstrapping system CRDs")

		if err := configshard.Bootstrap(goContext(ctx), s.KcpClusterClient.Cluster(systemShardCluster)); err != nil {
			// nolint:nilerr
			klog.Errorf("Failed to bootstrap the shard workspace: %v", err)
			return nil // don't klog.Fatal. This only happens when context is cancelled.
		}
		klog.Infof("Finished bootstrapping the shard workspace")

		go s.KcpSharedInformerFactory.Apis().V1alpha1().APIExports().Informer().Run(ctx.StopCh)
		go s.KcpSharedInformerFactory.Apis().V1alpha1().APIBindings().Informer().Run(ctx.StopCh)

		if err := wait.PollInfiniteWithContext(goContext(ctx), time.Millisecond*100, func(ctx context.Context) (bool, error) {
			exportsSynced := s.KcpSharedInformerFactory.Apis().V1alpha1().APIExports().Informer().HasSynced()
			bindingsSynced := s.KcpSharedInformerFactory.Apis().V1alpha1().APIBindings().Informer().HasSynced()
			return exportsSynced && bindingsSynced, nil
		}); err != nil {
			klog.Errorf("failed to start APIExport and/or APIBinding informers: %v", err)
			// nolint:nilerr
			return nil // don't klog.Fatal. This only happens when context is cancelled.
		}

		klog.Infof("Finished starting APIExport and APIBinding informers")

		if s.Options.Extra.ShardName == tenancyv1alpha1.RootShard {
			// bootstrap root workspace phase 0 only if we are on the root shard, no APIBinding resources yet
			if err := configrootphase0.Bootstrap(goContext(ctx),
				s.KcpClusterClient.Cluster(tenancyv1alpha1.RootCluster),
				s.ApiExtensionsClusterClient.Cluster(tenancyv1alpha1.RootCluster).Discovery(),
				s.DynamicClusterClient.Cluster(tenancyv1alpha1.RootCluster),
			); err != nil {
				// nolint:nilerr
				klog.Errorf("failed to bootstrap root workspace phase 0: %w", err)
				return nil // don't klog.Fatal. This only happens when context is cancelled.
			}
			klog.Infof("Bootstrapped root workspace phase 0")
		}

		klog.Infof("Getting kcp APIExport identities")

		if err := wait.PollImmediateInfiniteWithContext(goContext(ctx), time.Millisecond*500, func(ctx context.Context) (bool, error) {
			if err := s.resolveIdentities(ctx); err != nil {
				klog.V(3).Infof("failed to resolve identities, keeping trying: %v", err)
				return false, nil
			}
			return true, nil
		}); err != nil {
			klog.Errorf("failed to get or create identities: %v", err)
			// nolint:nilerr
			return nil // don't klog.Fatal. This only happens when context is cancelled.
		}

		klog.Infof("Finished getting kcp APIExport identities")

		s.KcpSharedInformerFactory.Start(ctx.StopCh)
		s.KcpSharedInformerFactory.WaitForCacheSync(ctx.StopCh)

		select {
		case <-ctx.StopCh:
			return nil // context closed, avoid reporting success below
		default:
		}

		klog.Infof("Finished starting (remaining) kcp informers")

		klog.Infof("Starting dynamic metadata informer worker")
		go s.DynamicDiscoverySharedInformerFactory.StartWorker(goContext(ctx))

		klog.Infof("Synced all informers. Ready to start controllers")
		close(s.syncedCh)

		if s.Options.Extra.ShardName == tenancyv1alpha1.RootShard {
			// the root ws is only present on the root shard
			klog.Infof("Starting bootstrapping root workspace phase 1")
			servingCert, _ := delegationChainHead.SecureServingInfo.Cert.CurrentCertKeyContent()
			if err := configroot.Bootstrap(goContext(ctx),
				s.ApiExtensionsClusterClient.Cluster(tenancyv1alpha1.RootCluster).Discovery(),
				s.DynamicClusterClient.Cluster(tenancyv1alpha1.RootCluster),
				s.Options.Extra.ShardName,
				clientcmdapi.Config{
					Clusters: map[string]*clientcmdapi.Cluster{
						// cross-cluster is the virtual cluster running by default
						"shard": {
							Server:                   "https://" + delegationChainHead.ExternalAddress,
							CertificateAuthorityData: servingCert, // TODO(sttts): wire controller updating this when it changes, or use CA
						},
					},
					Contexts: map[string]*clientcmdapi.Context{
						"shard": {Cluster: "shard"},
					},
					CurrentContext: "shard",
				},
				logicalcluster.New(s.Options.HomeWorkspaces.HomeRootPrefix).Base(),
				s.Options.HomeWorkspaces.HomeCreatorGroups,
			); err != nil {
				// nolint:nilerr
				klog.Errorf("failed to bootstrap root workspace phase 1: %w", err)
				return nil // don't klog.Fatal. This only happens when context is cancelled.
			}
			klog.Infof("Finished bootstrapping root workspace phase 1")
		}

		return nil
	}); err != nil {
		return err
	}

	// ========================================================================================================
	// TODO: split apart everything after this line, into their own commands, optional launched in this process

	controllerConfig := rest.CopyConfig(s.identityConfig)

	if err := s.installKubeNamespaceController(ctx, controllerConfig); err != nil {
		return err
	}

	if err := s.installClusterRoleAggregationController(ctx, controllerConfig); err != nil {
		return err
	}

	if err := s.installKubeServiceAccountController(ctx, controllerConfig); err != nil {
		return err
	}

	if err := s.installKubeServiceAccountTokenController(ctx, controllerConfig); err != nil {
		return err
	}

	if err := s.installRootCAConfigMapController(ctx, s.GenericControlPlane.GenericAPIServer.LoopbackClientConfig); err != nil {
		return err
	}

	enabled := sets.NewString(s.Options.Controllers.IndividuallyEnabled...)
	if len(enabled) > 0 {
		klog.Infof("Starting controllers individually: %v", enabled)
	}

	if s.Options.Controllers.EnableAll || enabled.Has("cluster") {
		// TODO(marun) Consider enabling each controller via a separate flag

		if err := s.installApiResourceController(ctx, controllerConfig); err != nil {
			return err
		}
		if err := s.installSyncTargetHeartbeatController(ctx, controllerConfig); err != nil {
			return err
		}
		if err := s.installVirtualWorkspaceURLsController(ctx, controllerConfig, delegationChainHead); err != nil {
			return err
		}
	}

	if s.Options.Controllers.EnableAll || enabled.Has("workspace-scheduler") {
		if err := s.installWorkspaceScheduler(ctx, controllerConfig); err != nil {
			return err
		}
		if err := s.installWorkspaceDeletionController(ctx, controllerConfig); err != nil {
			return err
		}
	}

	if s.Options.HomeWorkspaces.Enabled {
		if err := s.installHomeWorkspaces(ctx, controllerConfig); err != nil {
			return err
		}
	}

	if s.Options.Controllers.EnableAll || enabled.Has("resource-scheduler") {
		if err := s.installWorkloadResourceScheduler(ctx, controllerConfig, s.DynamicDiscoverySharedInformerFactory); err != nil {
			return err
		}
	}

	if s.Options.Controllers.EnableAll || enabled.Has("apibinding") {
		if err := s.installAPIBindingController(ctx, controllerConfig, delegationChainHead, s.DynamicDiscoverySharedInformerFactory); err != nil {
			return err
		}
	}

	if s.Options.Controllers.EnableAll || enabled.Has("apiexport") {
		if err := s.installAPIExportController(ctx, controllerConfig, delegationChainHead); err != nil {
			return err
		}
	}

	if kcpfeatures.DefaultFeatureGate.Enabled(kcpfeatures.LocationAPI) {
		if s.Options.Controllers.EnableAll || enabled.Has("scheduling") {
			if err := s.installWorkloadNamespaceScheduler(ctx, controllerConfig, delegationChainHead); err != nil {
				return err
			}
			if err := s.installWorkloadPlacementScheduler(ctx, controllerConfig, delegationChainHead); err != nil {
				return err
			}
			if err := s.installSchedulingLocationStatusController(ctx, controllerConfig, delegationChainHead); err != nil {
				return err
			}
			if err := s.installSchedulingPlacementController(ctx, controllerConfig, delegationChainHead); err != nil {
				return err
			}
			if err := s.installWorkloadsAPIExportController(ctx, controllerConfig, delegationChainHead); err != nil {
				return err
			}
			if err := s.installWorkloadsAPIExportCreateController(ctx, controllerConfig, delegationChainHead); err != nil {
				return err
			}
			if err := s.installDefaultPlacementController(ctx, controllerConfig, delegationChainHead); err != nil {
				return err
			}
		}
	}

	if s.Options.Controllers.EnableAll || enabled.Has("quota") {
		if err := s.installKubeQuotaController(ctx, controllerConfig, delegationChainHead); err != nil {
			return err
		}
	}

	if s.Options.Virtual.Enabled {
		if err := s.installVirtualWorkspaces(ctx, controllerConfig, delegationChainHead, s.GenericConfig.Authentication, s.GenericConfig.ExternalAddress, s.preHandlerChainMux); err != nil {
			return err
		}
	} else if err := s.installVirtualWorkspacesRedirect(ctx, s.preHandlerChainMux); err != nil {
		return err
	}

	if err := s.Options.AdminAuthentication.WriteKubeConfig(s.GenericConfig, s.kcpAdminToken, s.shardAdminToken, s.shardAdminTokenHash); err != nil {
		return err
	}

	return delegationChainHead.PrepareRun().Run(ctx.Done())
}

// goContext turns the PostStartHookContext into a context.Context for use in routines that may or may not
// run inside of a post-start-hook. The k8s APIServer wrote the post-start-hook context code before contexts
// were part of the Go stdlib.
func goContext(parent genericapiserver.PostStartHookContext) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func(done <-chan struct{}) {
		<-done
		cancel()
	}(parent.StopCh)
	return ctx
}

type handlerChainMuxes []*http.ServeMux

func (mxs *handlerChainMuxes) Handle(pattern string, handler http.Handler) {
	for _, mx := range *mxs {
		mx.Handle(pattern, handler)
	}
}

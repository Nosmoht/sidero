// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package capi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"time"

	cacpt "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	"github.com/siderolabs/go-retry/retry"
	taloscluster "github.com/siderolabs/talos/pkg/cluster"
	talosclusterapi "github.com/siderolabs/talos/pkg/machinery/api/cluster"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Cluster attaches to the provisioned CAPI cluster and provides talos.Cluster.
type Cluster struct {
	name              string
	controlPlaneNodes []string
	workerNodes       []string
	bridgeIP          netip.Addr
	client            *talosclient.Client
	k8sProvider       *taloscluster.KubernetesClient
}

// NewCluster fetches cluster info from the CAPI state.
func NewCluster(ctx context.Context, metalClient runtimeclient.Reader, clusterName string, bridgeIP netip.Addr) (*Cluster, error) {
	var (
		cluster            capiv1.Cluster
		controlPlane       cacpt.TalosControlPlane
		machines           capiv1.MachineList
		machineDeployments capiv1.MachineDeploymentList
		talosSecret        v1.Secret
	)

	if err := metalClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: clusterName}, &cluster); err != nil {
		return nil, err
	}

	if err := metalClient.Get(ctx, types.NamespacedName{Namespace: cluster.Spec.ControlPlaneRef.Namespace, Name: cluster.Spec.ControlPlaneRef.Name}, &controlPlane); err != nil {
		return nil, err
	}

	labelSelector, err := labels.Parse(controlPlane.Status.Selector)
	if err != nil {
		return nil, err
	}

	if err = metalClient.List(ctx, &machines, runtimeclient.MatchingLabelsSelector{Selector: labelSelector}); err != nil {
		return nil, err
	}

	if len(machines.Items) < 1 {
		return nil, fmt.Errorf("not enough machines found")
	}

	if err = metalClient.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: fmt.Sprintf("%s-talosconfig", cluster.Name)}, &talosSecret); err != nil {
		return nil, err
	}

	config, ok := talosSecret.Data["talosconfig"]
	if !ok {
		return nil, fmt.Errorf("failed to find talosconfig data in the talosconfig secret")
	}

	var clientConfig *clientconfig.Config

	clientConfig, err = clientconfig.FromBytes(config)
	if err != nil {
		return nil, err
	}

	resolveMachinesToIPs := func(machines capiv1.MachineList) []string {
		var endpoints []string

		for _, machine := range machines.Items {
			if !machine.DeletionTimestamp.IsZero() {
				continue
			}

			if capiv1.MachinePhase(machine.Status.Phase) != capiv1.MachinePhaseRunning && capiv1.MachinePhase(machine.Status.Phase) != capiv1.MachinePhaseProvisioned {
				continue
			}

			for _, address := range machine.Status.Addresses {
				if address.Type == capiv1.MachineInternalIP {
					endpoints = append(endpoints, address.Address)
				}
			}
		}

		return endpoints
	}

	controlPlaneNodes := resolveMachinesToIPs(machines)

	if len(controlPlaneNodes) < 1 {
		return nil, fmt.Errorf("failed to find control plane nodes")
	}

	if err = metalClient.List(ctx, &machineDeployments, runtimeclient.MatchingLabels{"cluster.x-k8s.io/cluster-name": clusterName}); err != nil {
		return nil, err
	}

	if len(machineDeployments.Items) != 1 {
		return nil, fmt.Errorf("unexpected number of machine deployments: %d", len(machineDeployments.Items))
	}

	labelSelector, err = labels.Parse(machineDeployments.Items[0].Status.Selector)
	if err != nil {
		return nil, err
	}

	if err = metalClient.List(ctx, &machines, runtimeclient.MatchingLabelsSelector{Selector: labelSelector}); err != nil {
		return nil, err
	}

	workerNodes := resolveMachinesToIPs(machines)

	// TODO: endpoints in talosconfig should be filled by Sidero
	clientConfig.Contexts[clientConfig.Context].Endpoints = controlPlaneNodes

	var talosClient *talosclient.Client

	talosClient, err = talosclient.New(ctx, talosclient.WithConfig(clientConfig))
	if err != nil {
		return nil, err
	}

	return &Cluster{
		name:              clusterName,
		controlPlaneNodes: controlPlaneNodes,
		workerNodes:       workerNodes,
		bridgeIP:          bridgeIP,
		client:            talosClient,
		k8sProvider: &taloscluster.KubernetesClient{
			ClientProvider: &taloscluster.ConfigClientProvider{
				DefaultClient: talosClient,
			},
		},
	}, nil
}

// Health runs the healthcheck for the cluster.
func (cluster *Cluster) Health(ctx context.Context) error {
	return retry.Constant(5*time.Minute, retry.WithUnits(10*time.Second)).Retry(func() error {
		// retry health checks as sometimes bootstrap bootkube issues break the check
		return retry.ExpectedError(cluster.health(ctx))
	})
}

func (cluster *Cluster) health(ctx context.Context) error {
	resp, err := cluster.client.ClusterHealthCheck(talosclient.WithNodes(ctx, cluster.controlPlaneNodes[0]), 3*time.Minute, &talosclusterapi.ClusterInfo{
		ControlPlaneNodes: cluster.controlPlaneNodes,
		WorkerNodes:       cluster.workerNodes,
	})
	if err != nil {
		return err
	}

	if err := resp.CloseSend(); err != nil {
		return err
	}

	for {
		msg, err := resp.Recv()
		if err != nil {
			if err == io.EOF || status.Code(err) == codes.Canceled {
				return nil
			}

			return err
		}

		if msg.GetMetadata().GetError() != "" {
			return fmt.Errorf("healthcheck error: %s", msg.GetMetadata().GetError())
		}

		fmt.Fprintln(os.Stderr, msg.GetMessage())
	}
}

// Name of the cluster.
func (cluster *Cluster) Name() string {
	return cluster.name
}

// BridgeIP returns IP of the bridge which controls the cluster.
func (cluster *Cluster) BridgeIP() netip.Addr {
	return cluster.bridgeIP
}

// SideroComponentsIP returns the IP for the Sidero components (TFTP, iPXE, etc.).
func (cluster *Cluster) SideroComponentsIP() net.IP {
	panic("not implemented yet")
}

// KubernetesClient provides K8s client source.
func (cluster *Cluster) KubernetesClient() taloscluster.K8sProvider {
	return cluster.k8sProvider
}

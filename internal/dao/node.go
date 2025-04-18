// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package dao

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/render"
	"github.com/derailed/k9s/internal/slogs"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"
	"k8s.io/kubectl/pkg/scheme"
	mv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

var (
	_ Accessor       = (*Node)(nil)
	_ NodeMaintainer = (*Node)(nil)
)

// NodeMetricsFunc retrieves node metrics.
type NodeMetricsFunc func() (*mv1beta1.NodeMetricsList, error)

// Node represents a node model.
type Node struct {
	Resource
}

// ToggleCordon toggles cordon/uncordon a node.
func (n *Node) ToggleCordon(fqn string, cordon bool) error {
	slog.Debug("Toggle cordon on node",
		slogs.GVR, n.GVR(),
		slogs.FQN, fqn,
		slogs.Bool, cordon,
	)
	o, err := FetchNode(context.Background(), n.Factory, fqn)
	if err != nil {
		return err
	}

	h, err := drain.NewCordonHelperFromRuntimeObject(o, scheme.Scheme, n.gvr.GVK())
	if err != nil {
		slog.Debug("Fail to toggle cordon on node",
			slogs.FQN, fqn,
			slogs.Error, err,
		)
		return err
	}

	if !h.UpdateIfRequired(cordon) {
		if cordon {
			return fmt.Errorf("node is already cordoned")
		}
		return fmt.Errorf("node is already uncordoned")
	}
	dial, err := n.getFactory().Client().Dial()
	if err != nil {
		return err
	}

	err, patchErr := h.PatchOrReplace(dial, false)
	if patchErr != nil {
		return patchErr
	}
	if err != nil {
		return err
	}

	return nil
}

func (o DrainOptions) toDrainHelper(k kubernetes.Interface, w io.Writer) drain.Helper {
	return drain.Helper{
		Client:              k,
		GracePeriodSeconds:  o.GracePeriodSeconds,
		Timeout:             o.Timeout,
		DeleteEmptyDirData:  o.DeleteEmptyDirData,
		IgnoreAllDaemonSets: o.IgnoreAllDaemonSets,
		DisableEviction:     o.DisableEviction,
		Out:                 w,
		ErrOut:              w,
		Force:               o.Force,
	}
}

// Drain drains a node.
func (n *Node) Drain(path string, opts DrainOptions, w io.Writer) error {
	cordoned, err := n.ensureCordoned(path)
	if err != nil {
		return err
	}

	if !cordoned {
		if e := n.ToggleCordon(path, true); e != nil {
			return e
		}
	}

	dial, err := n.getFactory().Client().Dial()
	if err != nil {
		return err
	}
	h := opts.toDrainHelper(dial, w)
	dd, errs := h.GetPodsForDeletion(path)
	if len(errs) != 0 {
		for _, e := range errs {
			if _, err := fmt.Fprintf(h.ErrOut, "[%s] %s\n", path, e.Error()); err != nil {
				return err
			}
		}
		return errors.Join(errs...)
	}

	if err := h.DeleteOrEvictPods(dd.Pods()); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(h.Out, "Node %s drained!", path)

	return nil
}

// Get returns a node resource.
func (n *Node) Get(ctx context.Context, path string) (runtime.Object, error) {
	oo, err := n.Resource.List(ctx, "")
	if err != nil {
		return nil, err
	}

	var raw *unstructured.Unstructured
	for _, o := range oo {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetName() == path {
			raw = u
		}
	}
	if raw == nil {
		return nil, fmt.Errorf("unable to locate node %s", path)
	}

	var nmx *mv1beta1.NodeMetrics
	if withMx, ok := ctx.Value(internal.KeyWithMetrics).(bool); ok && withMx {
		nmx, _ = client.DialMetrics(n.Client()).FetchNodeMetrics(ctx, path)
	}

	return &render.NodeWithMetrics{Raw: raw, MX: nmx}, nil
}

// List returns a collection of node resources.
func (n *Node) List(ctx context.Context, ns string) ([]runtime.Object, error) {
	oo, err := n.Resource.List(ctx, ns)
	if err != nil {
		return oo, err
	}

	var nmx client.NodesMetricsMap
	if withMx, ok := ctx.Value(internal.KeyWithMetrics).(bool); withMx || !ok {
		nmx, _ = client.DialMetrics(n.Client()).FetchNodesMetricsMap(ctx)
	}

	shouldCountPods, _ := ctx.Value(internal.KeyPodCounting).(bool)
	var pods []runtime.Object
	if shouldCountPods {
		pods, err = n.getFactory().List(client.PodGVR, client.BlankNamespace, false, labels.Everything())
		if err != nil {
			slog.Error("Unable to list pods", slogs.Error, err)
		}
	}
	res := make([]runtime.Object, 0, len(oo))
	for _, o := range oo {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			return res, fmt.Errorf("expecting *unstructured.Unstructured but got `%T", o)
		}

		fqn := extractFQN(o)
		_, name := client.Namespaced(fqn)
		podCount := -1
		if shouldCountPods {
			podCount, err = n.CountPods(pods, name)
			if err != nil {
				slog.Error("Unable to get pods count",
					slogs.ResName, name,
					slogs.Error, err,
				)
			}
		}
		res = append(res, &render.NodeWithMetrics{
			Raw:      u,
			MX:       nmx[name],
			PodCount: podCount,
		})
	}

	return res, nil
}

// CountPods counts the pods scheduled on a given node.
func (*Node) CountPods(oo []runtime.Object, nodeName string) (int, error) {
	var count int
	for _, o := range oo {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			return count, fmt.Errorf("expecting *Unstructured but got `%T", o)
		}
		spec, ok := u.Object["spec"].(map[string]any)
		if !ok {
			return count, fmt.Errorf("expecting spec interface map but got `%T", o)
		}
		if node, ok := spec["nodeName"]; ok && node == nodeName {
			count++
		}
	}

	return count, nil
}

// GetPods returns all pods running on given node.
func (n *Node) GetPods(nodeName string) ([]*v1.Pod, error) {
	oo, err := n.getFactory().List(client.PodGVR, client.BlankNamespace, false, labels.Everything())
	if err != nil {
		return nil, err
	}

	pp := make([]*v1.Pod, 0, len(oo))
	for _, o := range oo {
		po := new(v1.Pod)
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.(*unstructured.Unstructured).Object, po); err != nil {
			return nil, err
		}
		if po.Spec.NodeName == nodeName {
			pp = append(pp, po)
		}
	}

	return pp, nil
}

// ensureCordoned returns whether the given node has been cordoned
func (n *Node) ensureCordoned(path string) (bool, error) {
	o, err := FetchNode(context.Background(), n.Factory, path)
	if err != nil {
		return false, err
	}

	return o.Spec.Unschedulable, nil
}

// ----------------------------------------------------------------------------
// Helpers...

// FetchNode retrieves a node.
func FetchNode(_ context.Context, f Factory, path string) (*v1.Node, error) {
	_, n := client.Namespaced(path)
	auth, err := f.Client().CanI(client.ClusterScope, client.NodeGVR, n, client.GetAccess)
	if err != nil {
		return nil, err
	}
	if !auth {
		return nil, fmt.Errorf("user is not authorized to list nodes")
	}

	o, err := f.Get(client.NodeGVR, client.FQN(client.ClusterScope, path), true, labels.Everything())
	if err != nil {
		return nil, err
	}

	var node v1.Node
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(o.(*unstructured.Unstructured).Object, &node)
	if err != nil {
		return nil, err
	}

	return &node, nil
}

// FetchNodes retrieves all nodes.
func FetchNodes(_ context.Context, f Factory, _ string) (*v1.NodeList, error) {
	auth, err := f.Client().CanI(client.ClusterScope, client.NodeGVR, "", client.ListAccess)
	if err != nil {
		return nil, err
	}
	if !auth {
		return nil, fmt.Errorf("user is not authorized to list nodes")
	}

	oo, err := f.List(client.NodeGVR, "", false, labels.Everything())
	if err != nil {
		return nil, err
	}
	nn := make([]v1.Node, 0, len(oo))
	for _, o := range oo {
		var node v1.Node
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(o.(*unstructured.Unstructured).Object, &node)
		if err != nil {
			return nil, err
		}
		nn = append(nn, node)
	}

	return &v1.NodeList{Items: nn}, nil
}

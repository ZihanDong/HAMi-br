// Copyright 2025 The HAMi Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command biren-svi-manager is a per-node controller that makes Biren SVI
// partitioning DYNAMIC, analogous to NVIDIA dynamic-MIG.
//
// The HAMi Biren backend (pkg/device/biren) only SCHEDULES pre-existing SVI
// instances; SVI mode itself (`brsmi gpu set -s`) is a persistent hardware
// setting that nothing reconciles, so a partitioned-but-idle GPU never reverts
// to a whole card on its own. This controller closes that loop:
//
//   - reclaim : when the node has no vGPU occupancy, idle "Enabled"
//     (partitioned) GPUs are reverted to a whole card (`-s 0`).
//   - provision: when a vGPU pod is pending and there is no free instance, an
//     idle whole GPU is partitioned to the matching mode
//     (1/2 -> -s 1, 1/4 -> -s 2).
//
// After a mode change the vendor device plugin re-advertises the new instance
// counts; this controller also keeps the HAMi node-registration annotations in
// sync each cycle so the scheduler's view tracks the live allocatable.
//
// Runs as a privileged DaemonSet on Biren GPU nodes (host brsmi + lib mounts,
// in-cluster ServiceAccount with node/pod read + node patch).
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// flavor maps a kubelet SVI resource to its HAMi commonWord, the brsmi mode
// that produces it, the instances a single physical GPU yields in that mode,
// and a representative per-instance memory (MiB) for node registration.
type flavor struct {
	resource   string
	commonWord string
	sviMode    int
	perGPU     int
	devmem     int32
}

var flavors = []flavor{
	{"birentech.com/gpu", "BirenGPU", 0, 1, 65536},       // brsmi -s 0 (whole)
	{"birentech.com/1-2-gpu", "Biren-1of2", 1, 2, 32768}, // brsmi -s 1 (2 instances)
	{"birentech.com/1-4-gpu", "Biren-1of4", 2, 4, 16384}, // brsmi -s 2 (4 instances)
}

type gpu struct {
	index   int
	enabled bool // SVI partitioned (Enabled) vs whole (Disabled)
	memUsed int  // MiB allocated by active contexts
}

type config struct {
	node      string
	brsmi     string
	interval  time.Duration
	grace     int
	provision bool
	reclaim   bool
	dryRun    bool
	dpNS      string // vendor device-plugin namespace
	dpLabel   string // vendor device-plugin pod label selector
	schedNS   string // hami-scheduler namespace
	schedLbl  string // hami-scheduler pod label selector
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	klog.InitFlags(nil)
	cfg := config{
		node:      env("NODE_NAME", ""),
		brsmi:     env("BRSMI", "brsmi"),
		grace:     atoiDef(env("GRACE_CYCLES", "2"), 2),
		provision: env("ENABLE_PROVISION", "true") == "true",
		reclaim:   env("ENABLE_RECLAIM", "true") == "true",
		dryRun:    env("DRY_RUN", "false") == "true",
		dpNS:      env("DEVICE_PLUGIN_NAMESPACE", "biren-gpu"),
		dpLabel:   env("DEVICE_PLUGIN_LABEL", "name=biren-device-plugin"),
		schedNS:   env("SCHEDULER_NAMESPACE", "hami-system"),
		schedLbl:  env("SCHEDULER_LABEL", "app.kubernetes.io/component=hami-scheduler"),
	}
	cfg.interval = time.Duration(atoiDef(env("INTERVAL_SECONDS", "20"), 20)) * time.Second
	if cfg.node == "" {
		klog.Fatal("NODE_NAME env is required (set via downward API)")
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("in-cluster config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		klog.Fatalf("clientset: %v", err)
	}

	klog.Infof("biren-svi-manager starting: node=%s interval=%s grace=%d provision=%v reclaim=%v dryRun=%v",
		cfg.node, cfg.interval, cfg.grace, cfg.provision, cfg.reclaim, cfg.dryRun)

	idleStreak := map[int]int{}
	for {
		if err := reconcile(cs, &cfg, idleStreak); err != nil {
			klog.ErrorS(err, "reconcile failed")
		}
		time.Sleep(cfg.interval)
	}
}

func atoiDef(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func reconcile(cs *kubernetes.Clientset, cfg *config, idleStreak map[int]int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	gpus, err := queryGPUs(cfg.brsmi)
	if err != nil {
		return fmt.Errorf("query gpus: %w", err)
	}

	node, err := cs.CoreV1().Nodes().Get(ctx, cfg.node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}
	alloc := map[string]int{}
	for _, f := range flavors {
		if q, ok := node.Status.Allocatable[corev1.ResourceName(f.resource)]; ok {
			alloc[f.resource] = int(q.Value())
		}
	}

	runningReq, pendingReq, err := demand(ctx, cs, cfg.node)
	if err != nil {
		return fmt.Errorf("demand: %w", err)
	}

	// Keep HAMi node-registration in sync with the live allocatable.
	if err := syncRegistration(ctx, cs, cfg, alloc); err != nil {
		klog.ErrorS(err, "sync registration")
	}

	changed := false

	// ── provision: a pending vGPU pod with no free instance of its flavor ──
	if cfg.provision {
		for _, f := range flavors {
			if f.sviMode == 0 {
				continue
			}
			free := alloc[f.resource] - runningReq[f.resource]
			if pendingReq[f.resource] > 0 && free <= 0 {
				if g := firstIdleWhole(gpus); g >= 0 {
					klog.Infof("provision: pending %s and no free instance -> set GPU %d to SVI mode %d", f.resource, g, f.sviMode)
					if setMode(cfg, g, f.sviMode) == nil {
						changed = true
					}
					break // one change per cycle; let the plugin re-advertise
				}
				klog.Infof("provision: pending %s but no idle whole GPU available", f.resource)
			}
		}
	}

	// ── reclaim: no vGPU occupancy at all -> revert idle partitioned GPUs ──
	if cfg.reclaim && !changed {
		totalVGPU := runningReq["birentech.com/1-2-gpu"] + runningReq["birentech.com/1-4-gpu"] +
			pendingReq["birentech.com/1-2-gpu"] + pendingReq["birentech.com/1-4-gpu"]
		for _, g := range gpus {
			if !g.enabled {
				idleStreak[g.index] = 0
				continue
			}
			if totalVGPU == 0 && g.memUsed == 0 {
				idleStreak[g.index]++
			} else {
				idleStreak[g.index] = 0
			}
			if idleStreak[g.index] >= cfg.grace {
				klog.Infof("reclaim: GPU %d partitioned+idle for %d cycles and no vGPU occupancy -> revert to whole", g.index, idleStreak[g.index])
				if setMode(cfg, g.index, 0) == nil {
					idleStreak[g.index] = 0
					changed = true
				}
			}
		}
	}

	if changed {
		// The vendor device plugin does not reliably re-advertise after a live
		// SVI mode change, and the HAMi scheduler does not evict cached devices
		// when their register annotation changes. Restart both on this node so
		// allocatable re-advertises and the scheduler re-ingests the live
		// instance set (otherwise it may bind pods onto instances that no longer
		// exist, or miss freshly-provisioned ones).
		restartPods(ctx, cs, cfg, cfg.dpNS, cfg.dpLabel, "device-plugin")
		restartPods(ctx, cs, cfg, cfg.schedNS, cfg.schedLbl, "hami-scheduler")
		klog.Info("SVI mode changed; restarted device-plugin + scheduler; registration re-syncs next cycle")
	}
	return nil
}

// restartPods deletes pods matching ns/label on this node so their controller
// recreates them with fresh state.
func restartPods(ctx context.Context, cs *kubernetes.Clientset, cfg *config, ns, label, desc string) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: label})
	if err != nil {
		klog.ErrorS(err, "list pods", "ns", ns, "label", label)
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != cfg.node {
			continue
		}
		if cfg.dryRun {
			klog.Infof("[dry-run] would restart %s pod %s/%s", desc, p.Namespace, p.Name)
			continue
		}
		if err := cs.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil {
			klog.ErrorS(err, "delete pod", "pod", p.Name)
		} else {
			klog.Infof("restarted %s pod %s/%s", desc, p.Namespace, p.Name)
		}
	}
}

// queryGPUs returns each physical GPU's SVI state and used memory via brsmi.
func queryGPUs(brsmi string) ([]gpu, error) {
	out, err := exec.Command(brsmi, "gpu",
		"--query-gpu=index,svi.mode.current,memory.used",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, err
	}
	var gpus []gpu
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		mode := strings.ToLower(strings.TrimSpace(fields[1]))
		mem, _ := strconv.Atoi(strings.TrimSpace(fields[2]))
		gpus = append(gpus, gpu{index: idx, enabled: mode == "enabled", memUsed: mem})
	}
	return gpus, nil
}

func firstIdleWhole(gpus []gpu) int {
	for _, g := range gpus {
		if !g.enabled && g.memUsed == 0 {
			return g.index
		}
	}
	return -1
}

func setMode(cfg *config, index, mode int) error {
	if cfg.dryRun {
		klog.Infof("[dry-run] brsmi gpu set -s %d -i %d", mode, index)
		return nil
	}
	out, err := exec.Command(cfg.brsmi, "gpu", "set", "-s", strconv.Itoa(mode), "-i", strconv.Itoa(index)).CombinedOutput()
	if err != nil {
		klog.ErrorS(err, "brsmi gpu set failed", "index", index, "mode", mode, "out", string(out))
		return err
	}
	klog.Infof("brsmi gpu set -s %d -i %d: %s", mode, index, strings.TrimSpace(string(out)))
	return nil
}

// demand sums vGPU resource requests by non-terminal pods: running counts pods
// bound to this node; pending counts unscheduled pods (cluster-wide) that want
// a vGPU but have no node yet.
func demand(ctx context.Context, cs *kubernetes.Clientset, node string) (running, pending map[string]int, err error) {
	running, pending = map[string]int{}, map[string]int{}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		req := podVGPURequests(p)
		if len(req) == 0 {
			continue
		}
		switch {
		case p.Spec.NodeName == node:
			for r, n := range req {
				running[r] += n
			}
		case p.Spec.NodeName == "" && p.Status.Phase == corev1.PodPending:
			for r, n := range req {
				pending[r] += n
			}
		}
	}
	return running, pending, nil
}

func podVGPURequests(p *corev1.Pod) map[string]int {
	out := map[string]int{}
	for i := range p.Spec.Containers {
		lim := p.Spec.Containers[i].Resources.Limits
		for _, f := range flavors {
			if f.sviMode == 0 {
				continue
			}
			if q, ok := lim[corev1.ResourceName(f.resource)]; ok {
				out[f.resource] += int(q.Value())
			}
		}
	}
	return out
}

// syncRegistration publishes hami.io/node-<commonWord>-register (JSON
// []DeviceInfo, Count=1 each) from the live allocatable and clears stale
// handshakes so the scheduler re-ingests after mode changes.
func syncRegistration(ctx context.Context, cs *kubernetes.Clientset, cfg *config, alloc map[string]int) error {
	annos := map[string]interface{}{}
	for _, f := range flavors {
		n := alloc[f.resource]
		key := fmt.Sprintf("hami.io/node-%s-register", f.commonWord)
		if n <= 0 {
			annos[key] = nil // remove
			annos[fmt.Sprintf("hami.io/node-handshake-%s", f.commonWord)] = nil
			continue
		}
		var sb strings.Builder
		sb.WriteByte('[')
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"id":"%s-%s-%d","index":%d,"count":1,"devmem":%d,"devcore":100,"type":"%s","numa":0,"mode":"biren-svi","health":true}`,
				f.commonWord, cfg.node, i, i, f.devmem, f.commonWord)
		}
		sb.WriteByte(']')
		annos[key] = sb.String()
		// clear handshake so a (re)started scheduler re-ingests
		annos[fmt.Sprintf("hami.io/node-handshake-%s", f.commonWord)] = nil
	}
	return patchNodeAnnotations(ctx, cs, cfg.node, annos)
}

func patchNodeAnnotations(ctx context.Context, cs *kubernetes.Clientset, node string, annos map[string]interface{}) error {
	var sb strings.Builder
	sb.WriteString(`{"metadata":{"annotations":{`)
	first := true
	for k, v := range annos {
		if !first {
			sb.WriteByte(',')
		}
		first = false
		if v == nil {
			fmt.Fprintf(&sb, "%q:null", k)
		} else {
			fmt.Fprintf(&sb, "%q:%q", k, v.(string))
		}
	}
	sb.WriteString(`}}}`)
	_, err := cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, []byte(sb.String()), metav1.PatchOptions{})
	return err
}

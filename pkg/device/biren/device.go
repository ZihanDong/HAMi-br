/*
Copyright 2025 The HAMi Authors.

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

package biren

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/common"
	"github.com/Project-HAMi/HAMi/pkg/util"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// BirenNodeLock is the per-node mutex annotation key shared by Biren backends.
	BirenNodeLock = "hami.io/mutex.lock"
	// BirenVisibleDevicesEnv is the env consumed by the Biren runtime to pin a
	// container to a set of (SVI) cards. Mirrors the Biren device plugin.
	BirenVisibleDevicesEnv = "BR_PHY_CARDS"
)

// BirenDevices implements device.Devices for a single Biren SVI flavor.
//
// SVI instances are hardware-isolated whole devices, so allocation is exclusive:
// every registered instance has Count == 1 and a used instance cannot be reused.
type BirenDevices struct {
	config           BirenConfig
	nodeRegisterAnno string
	useUUIDAnno      string
	noUseUUIDAnno    string
	handshakeAnno    string
}

// InitBirenDevices builds one BirenDevices per configured flavor. It returns an
// empty slice when the backend is disabled or no flavor is configured.
func InitBirenDevices(cfgs BirenConfigs) []*BirenDevices {
	var devs []*BirenDevices
	if !enableBiren {
		return devs
	}
	for _, cfg := range cfgs.Configs {
		if cfg.CommonWord == "" || cfg.ResourceCountName == "" {
			klog.Errorf("skip invalid biren config (missing commonWord or resourceCountName): %+v", cfg)
			continue
		}
		commonWord := cfg.CommonWord
		dev := &BirenDevices{
			config:           cfg,
			nodeRegisterAnno: fmt.Sprintf("hami.io/node-%s-register", commonWord),
			useUUIDAnno:      fmt.Sprintf("hami.io/use-%s-uuid", commonWord),
			noUseUUIDAnno:    fmt.Sprintf("hami.io/no-use-%s-uuid", commonWord),
			handshakeAnno:    fmt.Sprintf("hami.io/node-handshake-%s", commonWord),
		}
		if _, ok := device.InRequestDevices[commonWord]; !ok {
			device.InRequestDevices[commonWord] = fmt.Sprintf("hami.io/%s-devices-to-allocate", commonWord)
			device.SupportDevices[commonWord] = fmt.Sprintf("hami.io/%s-devices-allocated", commonWord)
			util.HandshakeAnnos[commonWord] = dev.handshakeAnno
		}
		devs = append(devs, dev)
		klog.Infof("load biren svi config %s: %+v", commonWord, dev.config)
	}
	return devs
}

func (dev *BirenDevices) CommonWord() string {
	return dev.config.CommonWord
}

// MutateAdmission validates the request and, when configured, applies the
// runtime class / hides host GPUs from unrelated containers. Memory is fixed by
// the SVI mode so nothing is rewritten here.
func (dev *BirenDevices) MutateAdmission(ctr *corev1.Container, p *corev1.Pod) (bool, error) {
	_, ok := ctr.Resources.Limits[corev1.ResourceName(dev.config.ResourceCountName)]
	if !ok {
		if dev.config.OverwriteEnv {
			ctr.Env = append(ctr.Env, corev1.EnvVar{Name: BirenVisibleDevicesEnv, Value: ""})
		}
		return false, nil
	}
	if dev.config.RuntimeClassName != "" && p.Spec.RuntimeClassName == nil {
		rc := dev.config.RuntimeClassName
		p.Spec.RuntimeClassName = &rc
	}
	return true, nil
}

// GetNodeDevices reads the device list the Biren device plugin published on the
// node annotation. Each entry is one SVI instance (or a whole GPU).
func (dev *BirenDevices) GetNodeDevices(n corev1.Node) ([]*device.DeviceInfo, error) {
	anno, ok := n.Annotations[dev.nodeRegisterAnno]
	if !ok {
		return []*device.DeviceInfo{}, fmt.Errorf("annos not found %s", dev.nodeRegisterAnno)
	}
	nodeDevices, err := device.UnMarshalNodeDevices(anno)
	if err != nil {
		klog.ErrorS(err, "failed to unmarshal node devices", "node", n.Name, "device annotation", anno)
		return []*device.DeviceInfo{}, err
	}
	for idx := range nodeDevices {
		nodeDevices[idx].DeviceVendor = dev.config.CommonWord
	}
	if len(nodeDevices) == 0 {
		klog.InfoS("no biren device found", "node", n.Name, "device annotation", anno)
		return []*device.DeviceInfo{}, errors.New("no device found on node")
	}
	return nodeDevices, nil
}

func (dev *BirenDevices) PatchAnnotations(pod *corev1.Pod, annoinput *map[string]string, pd device.PodDevices) map[string]string {
	commonWord := dev.CommonWord()
	devList, ok := pd[commonWord]
	if ok && len(devList) > 0 {
		deviceStr := device.EncodePodSingleDevice(devList)
		(*annoinput)[device.InRequestDevices[commonWord]] = deviceStr
		(*annoinput)[device.SupportDevices[commonWord]] = deviceStr
		klog.V(5).Infof("pod add annotation key [%s], values is [%s]", device.InRequestDevices[commonWord], deviceStr)
		klog.V(5).Infof("pod add annotation key [%s], values is [%s]", device.SupportDevices[commonWord], deviceStr)
	}
	return *annoinput
}

// LockNode is intentionally a no-op for Biren SVI.
//
// HAMi's per-node bind mutex (hami.io/mutex.lock) is held by the scheduler's
// Bind on success and is meant to be released by HAMi's OWN device plugin
// during Allocate. Biren SVI uses the vendor device plugin (which is not
// HAMi-aware and never releases the lock), so holding it would serialize all
// binds on the node for the full lock TTL (5m), blocking a second SVI pod.
//
// The lock is unnecessary here: SVI instances are whole, exclusive devices;
// kube-scheduler invokes Filter sequentially (so instance selection, recorded
// in the scheduler's podManager during Filter, never races), and the kubelet's
// extended-resource accounting plus the vendor plugin prevent double-allocation.
func (dev *BirenDevices) LockNode(n *corev1.Node, p *corev1.Pod) error {
	return nil
}

func (dev *BirenDevices) ReleaseNodeLock(n *corev1.Node, p *corev1.Pod) error {
	return nil
}

func (dev *BirenDevices) NodeCleanUp(nn string) error {
	return util.MarkAnnotationsToDelete(dev.handshakeAnno, nn)
}

func (dev *BirenDevices) checkType(d device.DeviceUsage, n device.ContainerDeviceRequest) bool {
	return strings.Compare(n.Type, dev.CommonWord()) == 0
}

func (dev *BirenDevices) CheckHealth(devType string, n *corev1.Node) (bool, bool) {
	return device.CheckHealth(devType, dev.GetResourceNames().ResourceCountName, n)
}

// GenerateResourceRequests reads the per-container instance count and an optional
// minimum memory filter. Cores are not requestable: an SVI instance is exclusive,
// so allocation always consumes the whole instance (Coresreq = 100).
func (dev *BirenDevices) GenerateResourceRequests(ctr *corev1.Container) device.ContainerDeviceRequest {
	countName := corev1.ResourceName(dev.config.ResourceCountName)
	v, ok := ctr.Resources.Limits[countName]
	if !ok {
		v, ok = ctr.Resources.Requests[countName]
	}
	if !ok {
		return device.ContainerDeviceRequest{}
	}
	n, ok := v.AsInt64()
	if !ok || n <= 0 {
		return device.ContainerDeviceRequest{}
	}

	memReq := int64(0)
	if dev.config.ResourceMemoryName != "" {
		mem, ok := ctr.Resources.Limits[corev1.ResourceName(dev.config.ResourceMemoryName)]
		if !ok {
			mem, ok = ctr.Resources.Requests[corev1.ResourceName(dev.config.ResourceMemoryName)]
		}
		if ok {
			if m, ok := mem.AsInt64(); ok {
				memReq = m
			}
		}
	}

	memPercent := int32(101)
	if memReq == 0 {
		// No explicit memory filter: any instance fits.
		memPercent = 101
	}
	klog.V(3).Infof("counting biren devices for container %s: count=%d, minMem=%dMiB", ctr.Name, n, memReq)
	return device.ContainerDeviceRequest{
		Nums:             int32(n),
		Type:             dev.CommonWord(),
		Memreq:           int32(memReq),
		MemPercentagereq: memPercent,
		Coresreq:         100,
	}
}

func (dev *BirenDevices) ScoreNode(node *corev1.Node, podDevices device.PodSingleDevice, previous []*device.DeviceUsage, policy string) float32 {
	return 0
}

func (dev *BirenDevices) AddResourceUsage(pod *corev1.Pod, n *device.DeviceUsage, ctr *device.ContainerDevice) error {
	n.Used++
	n.Usedcores += ctr.Usedcores
	n.Usedmem += ctr.Usedmem
	return nil
}

func (dev *BirenDevices) GetResourceNames() device.ResourceNames {
	return device.ResourceNames{
		ResourceCountName:  dev.config.ResourceCountName,
		ResourceMemoryName: dev.config.ResourceMemoryName,
		ResourceCoreName:   "",
	}
}

// Fit selects request.Nums free SVI instances. Each instance is exclusive
// (Count == 1): a used instance is skipped, and the whole instance memory is
// recorded as consumed so usage bookkeeping stays consistent.
func (dev *BirenDevices) Fit(devices []*device.DeviceUsage, request device.ContainerDeviceRequest, pod *corev1.Pod, nodeInfo *device.NodeInfo, allocated *device.PodDevices) (bool, map[string]device.ContainerDevices, string) {
	k := request
	originReq := k.Nums
	klog.InfoS("Allocating biren device for container request", "pod", klog.KObj(pod), "card request", k)
	tmpDevs := make(map[string]device.ContainerDevices)
	reason := make(map[string]int)

	for i, v := range slices.Backward(devices) {
		d := v
		klog.V(4).InfoS("scoring pod", "pod", klog.KObj(pod), "device", d.ID, "Memreq", k.Memreq, "Nums", k.Nums, "device index", i)

		if !dev.checkType(*d, k) {
			reason[common.CardTypeMismatch]++
			klog.V(5).InfoS(common.CardTypeMismatch, "pod", klog.KObj(pod), "device", d.ID, d.Type, k.Type)
			continue
		}
		if !d.Health {
			reason[common.CardNotHealth]++
			klog.V(5).InfoS(common.CardNotHealth, "pod", klog.KObj(pod), "device", d.ID, "health", d.Health)
			continue
		}
		if !device.CheckUUID(pod.GetAnnotations(), d.ID, dev.useUUIDAnno, dev.noUseUUIDAnno, dev.CommonWord()) {
			reason[common.CardUUIDMismatch]++
			klog.V(5).InfoS(common.CardUUIDMismatch, "pod", klog.KObj(pod), "device", d.ID, "current device info is:", *d)
			continue
		}
		// Whole-instance, exclusive: any used instance is fully occupied.
		if d.Used >= d.Count {
			reason[common.ExclusiveDeviceAllocateConflict]++
			klog.V(5).InfoS(common.ExclusiveDeviceAllocateConflict, "pod", klog.KObj(pod), "device", d.ID, "count", d.Count, "used", d.Used)
			continue
		}
		// Optional minimum-memory filter against the instance's fixed memory.
		if k.Memreq > 0 && d.Totalmem < k.Memreq {
			reason[common.CardInsufficientMemory]++
			klog.V(5).InfoS(common.CardInsufficientMemory, "pod", klog.KObj(pod), "device", d.ID, "device total memory", d.Totalmem, "request memory", k.Memreq)
			continue
		}

		if k.Nums > 0 {
			klog.V(5).InfoS("find fit biren device", "pod", klog.KObj(pod), "device", d.ID)
			k.Nums--
			tmpDevs[k.Type] = append(tmpDevs[k.Type], device.ContainerDevice{
				Idx:        int(d.Index),
				UUID:       d.ID,
				Type:       k.Type,
				Usedmem:    d.Totalmem,
				Usedcores:  100,
				CustomInfo: d.CustomInfo,
			})
		}
		if k.Nums == 0 {
			klog.V(4).InfoS("biren device allocate success", "pod", klog.KObj(pod), "allocate device", tmpDevs)
			return true, tmpDevs, ""
		}
	}
	if len(tmpDevs) > 0 {
		reason[common.AllocatedCardsInsufficientRequest] = len(tmpDevs[k.Type])
		klog.V(5).InfoS(common.AllocatedCardsInsufficientRequest, "pod", klog.KObj(pod), "request", originReq, "allocated", len(tmpDevs[k.Type]))
	}
	return false, tmpDevs, common.GenReason(reason, len(devices))
}

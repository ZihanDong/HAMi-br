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
	"testing"

	"github.com/Project-HAMi/HAMi/pkg/device"

	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newTestDev() *BirenDevices {
	return &BirenDevices{
		config: BirenConfig{
			CommonWord:         "Biren-1of2",
			ChipName:           "Biren166C",
			ResourceCountName:  "birentech.com/1-2-gpu",
			ResourceMemoryName: "birentech.com/1-2-gpumem",
		},
		nodeRegisterAnno: "hami.io/node-Biren-1of2-register",
		useUUIDAnno:      "hami.io/use-Biren-1of2-uuid",
		noUseUUIDAnno:    "hami.io/no-use-Biren-1of2-uuid",
		handshakeAnno:    "hami.io/node-handshake-Biren-1of2",
	}
}

func TestCommonWordAndResourceNames(t *testing.T) {
	dev := newTestDev()
	assert.Equal(t, dev.CommonWord(), "Biren-1of2")
	rn := dev.GetResourceNames()
	assert.Equal(t, rn.ResourceCountName, "birentech.com/1-2-gpu")
	assert.Equal(t, rn.ResourceMemoryName, "birentech.com/1-2-gpumem")
	assert.Equal(t, rn.ResourceCoreName, "")
}

func TestGetNodeDevices(t *testing.T) {
	dev := newTestDev()
	// Two SVI instances of a 64GB GPU put into mode1 (~32GB each), Count=1 each.
	in := []*device.DeviceInfo{
		{ID: "GPU-uuid-a-instance-0", Index: 0, Count: 1, Devmem: 32512, Devcore: 100, Type: "Biren-1of2", Numa: 0, Health: true},
		{ID: "GPU-uuid-a-instance-1", Index: 1, Count: 1, Devmem: 32512, Devcore: 100, Type: "Biren-1of2", Numa: 0, Health: true},
	}
	anno := device.MarshalNodeDevices(in)

	t.Run("exist devices", func(t *testing.T) {
		n := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "node-01",
				Annotations: map[string]string{dev.nodeRegisterAnno: anno},
			},
		}
		got, err := dev.GetNodeDevices(n)
		assert.NilError(t, err)
		assert.Equal(t, len(got), 2)
		assert.Equal(t, got[0].ID, "GPU-uuid-a-instance-0")
		assert.Equal(t, got[0].Devmem, int32(32512))
		assert.Equal(t, got[0].DeviceVendor, "Biren-1of2")
	})

	t.Run("missing annotation", func(t *testing.T) {
		n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-02"}}
		_, err := dev.GetNodeDevices(n)
		assert.Assert(t, err != nil)
	})
}

// TestGetNodeDevices_RealPluginOutput feeds the exact JSON produced by the
// companion Biren device plugin (biren-hami-deviceplugin discover, run against
// real Biren 166C hardware) into the scheduler-side parser, proving the
// plugin -> scheduler node-registration contract end to end.
func TestGetNodeDevices_RealPluginOutput(t *testing.T) {
	dev := &BirenDevices{
		config: BirenConfig{
			CommonWord:        "BirenGPU",
			ChipName:          "Biren166C",
			ResourceCountName: "birentech.com/gpu",
		},
		nodeRegisterAnno: "hami.io/node-BirenGPU-register",
	}
	// Captured verbatim from `biren-hami-deviceplugin -mode discover` on a host
	// with 4x Biren 166C, SVI disabled (each whole GPU = 65024 MiB).
	pluginJSON := `[{"id":"AP089906018","count":1,"devmem":65024,"devcore":100,"type":"BirenGPU","mode":"biren-svi","health":true},{"id":"AP092504006","index":1,"count":1,"devmem":65024,"devcore":100,"type":"BirenGPU","mode":"biren-svi","health":true},{"id":"AP089609015","index":2,"count":1,"devmem":65024,"devcore":100,"type":"BirenGPU","mode":"biren-svi","health":true},{"id":"RX000145002","index":3,"count":1,"devmem":65024,"devcore":100,"type":"BirenGPU","mode":"biren-svi","health":true}]`
	n := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "biren-host",
			Annotations: map[string]string{dev.nodeRegisterAnno: pluginJSON},
		},
	}
	got, err := dev.GetNodeDevices(n)
	assert.NilError(t, err)
	assert.Equal(t, len(got), 4)
	for _, d := range got {
		assert.Equal(t, d.Type, "BirenGPU")
		assert.Equal(t, d.Devmem, int32(65024))
		assert.Equal(t, d.Count, int32(1))
		assert.Equal(t, d.Mode, "biren-svi")
		assert.Equal(t, d.Health, true)
		assert.Equal(t, d.DeviceVendor, "BirenGPU")
	}
}

func TestGenerateResourceRequests(t *testing.T) {
	dev := newTestDev()
	tests := []struct {
		name string
		ctr  corev1.Container
		want device.ContainerDeviceRequest
	}{
		{
			name: "count only",
			ctr: corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"birentech.com/1-2-gpu": resource.MustParse("2"),
			}}},
			want: device.ContainerDeviceRequest{Nums: 2, Type: "Biren-1of2", Memreq: 0, MemPercentagereq: 101, Coresreq: 100},
		},
		{
			name: "count with memory filter",
			ctr: corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"birentech.com/1-2-gpu":    resource.MustParse("1"),
				"birentech.com/1-2-gpumem": resource.MustParse("16000"),
			}}},
			want: device.ContainerDeviceRequest{Nums: 1, Type: "Biren-1of2", Memreq: 16000, MemPercentagereq: 101, Coresreq: 100},
		},
		{
			name: "no request",
			ctr:  corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"cpu": resource.MustParse("1")}}},
			want: device.ContainerDeviceRequest{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dev.GenerateResourceRequests(&tt.ctr)
			assert.Equal(t, got, tt.want)
		})
	}
}

func usageList() []*device.DeviceUsage {
	return []*device.DeviceUsage{
		{ID: "inst-0", Index: 0, Count: 1, Used: 0, Totalmem: 32512, Usedmem: 0, Totalcore: 100, Type: "Biren-1of2", Health: true},
		{ID: "inst-1", Index: 1, Count: 1, Used: 0, Totalmem: 32512, Usedmem: 0, Totalcore: 100, Type: "Biren-1of2", Health: true},
		{ID: "inst-2", Index: 2, Count: 1, Used: 1, Totalmem: 32512, Usedmem: 32512, Totalcore: 100, Type: "Biren-1of2", Health: true},
	}
}

func TestFit(t *testing.T) {
	dev := newTestDev()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}

	t.Run("allocate one free instance", func(t *testing.T) {
		req := device.ContainerDeviceRequest{Nums: 1, Type: "Biren-1of2", Coresreq: 100, MemPercentagereq: 101}
		ok, alloc, _ := dev.Fit(usageList(), req, pod, &device.NodeInfo{}, &device.PodDevices{})
		assert.Assert(t, ok)
		assert.Equal(t, len(alloc["Biren-1of2"]), 1)
		// whole-instance: consumed memory equals the instance total.
		assert.Equal(t, alloc["Biren-1of2"][0].Usedmem, int32(32512))
		assert.Equal(t, alloc["Biren-1of2"][0].Usedcores, int32(100))
	})

	t.Run("allocate two, skipping the used one", func(t *testing.T) {
		req := device.ContainerDeviceRequest{Nums: 2, Type: "Biren-1of2", Coresreq: 100, MemPercentagereq: 101}
		ok, alloc, _ := dev.Fit(usageList(), req, pod, &device.NodeInfo{}, &device.PodDevices{})
		assert.Assert(t, ok)
		assert.Equal(t, len(alloc["Biren-1of2"]), 2)
		for _, d := range alloc["Biren-1of2"] {
			assert.Assert(t, d.UUID != "inst-2") // the occupied instance must not be chosen
		}
	})

	t.Run("insufficient free instances", func(t *testing.T) {
		req := device.ContainerDeviceRequest{Nums: 3, Type: "Biren-1of2", Coresreq: 100, MemPercentagereq: 101}
		ok, _, reason := dev.Fit(usageList(), req, pod, &device.NodeInfo{}, &device.PodDevices{})
		assert.Assert(t, !ok)
		assert.Assert(t, reason != "")
	})

	t.Run("type mismatch", func(t *testing.T) {
		req := device.ContainerDeviceRequest{Nums: 1, Type: "Biren-1of4", Coresreq: 100, MemPercentagereq: 101}
		ok, _, _ := dev.Fit(usageList(), req, pod, &device.NodeInfo{}, &device.PodDevices{})
		assert.Assert(t, !ok)
	})

	t.Run("memory filter excludes too-small instances", func(t *testing.T) {
		req := device.ContainerDeviceRequest{Nums: 1, Type: "Biren-1of2", Memreq: 64000, Coresreq: 100}
		ok, _, _ := dev.Fit(usageList(), req, pod, &device.NodeInfo{}, &device.PodDevices{})
		assert.Assert(t, !ok)
	})

	t.Run("unhealthy instance skipped", func(t *testing.T) {
		du := []*device.DeviceUsage{
			{ID: "inst-0", Index: 0, Count: 1, Totalmem: 32512, Totalcore: 100, Type: "Biren-1of2", Health: false},
		}
		req := device.ContainerDeviceRequest{Nums: 1, Type: "Biren-1of2", Coresreq: 100}
		ok, _, _ := dev.Fit(du, req, pod, &device.NodeInfo{}, &device.PodDevices{})
		assert.Assert(t, !ok)
	})
}

func TestFitWithUUIDSelector(t *testing.T) {
	dev := newTestDev()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:        "p",
		Annotations: map[string]string{dev.useUUIDAnno: "inst-1"},
	}}
	req := device.ContainerDeviceRequest{Nums: 1, Type: "Biren-1of2", Coresreq: 100, MemPercentagereq: 101}
	ok, alloc, _ := dev.Fit(usageList(), req, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Assert(t, ok)
	assert.Equal(t, len(alloc["Biren-1of2"]), 1)
	assert.Equal(t, alloc["Biren-1of2"][0].UUID, "inst-1")
}

func TestMutateAdmission(t *testing.T) {
	t.Run("requests resource -> handled", func(t *testing.T) {
		dev := newTestDev()
		ctr := &corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			"birentech.com/1-2-gpu": resource.MustParse("1"),
		}}}
		pod := &corev1.Pod{}
		handled, err := dev.MutateAdmission(ctr, pod)
		assert.NilError(t, err)
		assert.Assert(t, handled)
	})

	t.Run("no request with overwriteEnv hides GPUs", func(t *testing.T) {
		dev := newTestDev()
		dev.config.OverwriteEnv = true
		ctr := &corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"cpu": resource.MustParse("1")}}}
		pod := &corev1.Pod{}
		handled, err := dev.MutateAdmission(ctr, pod)
		assert.NilError(t, err)
		assert.Assert(t, !handled)
		found := false
		for _, e := range ctr.Env {
			if e.Name == BirenVisibleDevicesEnv && e.Value == "" {
				found = true
			}
		}
		assert.Assert(t, found)
	})

	t.Run("applies runtime class when unset", func(t *testing.T) {
		dev := newTestDev()
		dev.config.RuntimeClassName = "biren"
		ctr := &corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			"birentech.com/1-2-gpu": resource.MustParse("1"),
		}}}
		pod := &corev1.Pod{}
		_, err := dev.MutateAdmission(ctr, pod)
		assert.NilError(t, err)
		assert.Assert(t, pod.Spec.RuntimeClassName != nil)
		assert.Equal(t, *pod.Spec.RuntimeClassName, "biren")
	})
}

func TestAddResourceUsage(t *testing.T) {
	dev := newTestDev()
	n := &device.DeviceUsage{ID: "inst-0", Count: 1, Totalmem: 32512, Totalcore: 100}
	err := dev.AddResourceUsage(&corev1.Pod{}, n, &device.ContainerDevice{Usedmem: 32512, Usedcores: 100})
	assert.NilError(t, err)
	assert.Equal(t, n.Used, int32(1))
	assert.Equal(t, n.Usedmem, int32(32512))
	assert.Equal(t, n.Usedcores, int32(100))
}

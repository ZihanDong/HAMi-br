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

import "flag"

// BirenConfig describes one Biren SVI "flavor" exposed on a node.
//
// Biren SVI (Scalable Virtualization Interface) is a coarse-grained, mode-based
// hardware partition of a whole GPU configured out-of-band by the administrator
// (`brsmi gpu -s <0|1|2>`):
//
//	mode 0 / disabled -> 1 instance  (whole GPU)
//	mode 1            -> 2 instances (GPU split in half)
//	mode 2            -> 4 instances (GPU split in quarters)
//
// Each instance has its own fixed memory and is hardware-isolated, so HAMi
// schedules SVI instances as whole, exclusive devices (no memory/core
// oversubscription). Because instances of different modes are advertised by the
// kubelet under different resource names, each flavor is modelled as its own
// BirenConfig / device backend, mirroring Biren's own device plugin which uses
// resource names like "birentech.com/gpu", "birentech.com/1-2-gpu" and
// "birentech.com/1-4-gpu".
type BirenConfig struct {
	// CommonWord is the device type identifier used internally by HAMi
	// (e.g. "BirenGPU", "Biren-1of2", "Biren-1of4").
	CommonWord string `yaml:"commonWord"`
	// ChipName is the GPU model reported by BRML (e.g. "Biren166C").
	ChipName string `yaml:"chipName"`
	// ResourceCountName is the kubelet resource that carries the number of SVI
	// instances requested (e.g. "birentech.com/gpu").
	ResourceCountName string `yaml:"resourceCountName"`
	// ResourceMemoryName is optional. When set, a pod may request a minimum
	// per-instance memory (in MiB); Fit only considers instances whose total
	// memory satisfies the request. Memory is never oversubscribed.
	ResourceMemoryName string `yaml:"resourceMemoryName"`
	// OverwriteEnv, when true, injects an empty BR_VISIBLE_DEVICES env into
	// containers that do not request this resource, preventing them from seeing
	// host GPUs.
	OverwriteEnv bool `yaml:"overwriteEnv"`
	// RuntimeClassName, when set and not already specified on the pod, is applied
	// to pods requesting this resource.
	RuntimeClassName string `yaml:"runtimeClassName"`
}

// BirenConfigs is the top-level Biren configuration: a list of SVI flavors.
type BirenConfigs struct {
	Configs []BirenConfig `yaml:"configs"`
}

// enableBiren gates registration of the Biren backend(s) entirely.
var enableBiren bool

// ParseConfig registers Biren command-line flags onto fs.
func ParseConfig(fs *flag.FlagSet) {
	fs.BoolVar(&enableBiren, "enable-biren", false, "enable biren SVI device")
}

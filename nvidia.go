/*
 * Copyright (c) 2019, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"log"
	"os"
	"strings"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml" //到NVIDIA gpu-monitor-tool專案載入nvml.go

	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

const (
	envDisableHealthChecks = "DP_DISABLE_HEALTHCHECKS"
	allHealthChecks        = "xids"
)

type ResourceManager interface {
	Devices() []*pluginapi.Device
	CheckHealth(stop <-chan interface{}, devices []*pluginapi.Device, unhealthy chan<- *pluginapi.Device)
}

type GpuDeviceManager struct {}

func check(err error) {
	if err != nil {
		log.Panicln("Fatal:", err)
	}
}

func NewGpuDeviceManager() *GpuDeviceManager {
	return &GpuDeviceManager{}
}



func (g *GpuDeviceManager) Devices() []*pluginapi.Device {
	n, err := nvml.GetDeviceCount()   //用nvml.GetDeviceCount()獲得當前宿主機設備數，
	check(err)

	var devs []*pluginapi.Device  
	for i := uint(0); i < n; i++ {
		d, err := nvml.NewDeviceLite(i)  //將所有設備的信息加入devs數組，該數組每個成員是一個pluginapi.Device結構體，
		check(err)
		devs = append(devs, buildPluginDevice(d))
	}

	return devs
}

func (g *GpuDeviceManager) CheckHealth(stop <-chan interface{}, devices []*pluginapi.Device, unhealthy chan<- *pluginapi.Device) {
	checkHealth(stop, devices, unhealthy)
}

func buildPluginDevice(d *nvml.Device) *pluginapi.Device {
	dev := pluginapi.Device{  //ID被初始化爲每個設備的UUID，Health字段初始化爲"Healthy"
		ID:     d.UUID,
		Health: pluginapi.Healthy,
	}
	if d.CPUAffinity != nil {
		dev.Topology = &pluginapi.TopologyInfo{
			Nodes: []*pluginapi.NUMANode{
				&pluginapi.NUMANode{
					ID: int64(*(d.CPUAffinity)),
				},
			},
		}
	}
	return &dev
}

func checkHealth(stop <-chan interface{}, devices []*pluginapi.Device, unhealthy chan<- *pluginapi.Device) {
	disableHealthChecks := strings.ToLower(os.Getenv(envDisableHealthChecks))
	if disableHealthChecks == "all" {
		disableHealthChecks = allHealthChecks
	}
	if strings.Contains(disableHealthChecks, "xids") {
		return
	}

	eventSet := nvml.NewEventSet()
	defer nvml.DeleteEventSet(eventSet)

	for _, d := range devices { //爲每一個devs開啓驅動端健康檢測
		err := nvml.RegisterEventForDevice(eventSet, nvml.XidCriticalError, d.ID)  
		//爲每個設備開啓驅動端的健康監測，然後根據驅動返回的設備狀態碼決定是否要把不健康設備傳入NvidiaDevicePlugin的health check管道中。
		if err != nil && strings.HasSuffix(err.Error(), "Not Supported") {
			log.Printf("Warning: %s is too old to support healthchecking: %s. Marking it unhealthy.", d.ID, err)
			unhealthy <- d
			continue
		}
		check(err)
	}

	for {
		select {
		case <-stop:
			return
		default:
		}  //如果工作完成了就退出健康檢查

		e, err := nvml.WaitForEvent(eventSet, 5000)
		if err != nil && e.Etype != nvml.XidCriticalError {
			continue
		} //錯誤不是致命錯誤進行新一輪

		// FIXME: formalize the full list and document it.
		// http://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
		// Application errors: the GPU should still be healthy
		if e.Edata == 31 || e.Edata == 43 || e.Edata == 45 {
			continue
		} //健康的

		if e.UUID == nil || len(*e.UUID) == 0 {
			// All devices are unhealthy，將所有的設備號都放入xid——channel中並進行下一輪
			log.Printf("XidCriticalError: Xid=%d, All devices will go unhealthy.", e.Edata)
			for _, d := range devices {
				unhealthy <- d
			}
			continue
		}
       //有錯誤將所有有錯誤的設備都放進去
		for _, d := range devices {
			if d.ID == *e.UUID {
				log.Printf("XidCriticalError: Xid=%d on Device=%s, the device will go unhealthy.", e.Edata, d.ID)
				unhealthy <- d
			}
		}
	}
}

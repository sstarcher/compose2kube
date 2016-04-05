/*
Copyright 2015 Kelsey Hightower All rights reserved.
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

package main

import (
	"flag"
	"fmt"
	"github.com/ghodss/yaml"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/libcompose/project"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/api/unversioned"
	api "k8s.io/kubernetes/pkg/api/v1"
	batchv1 "k8s.io/kubernetes/pkg/apis/batch/v1"
)

var (
	composeFile  string
	outputDir    string
	pullPolicy   string
	nodeSelector string
)

func init() {
	flag.StringVar(&composeFile, "compose-file", "docker-compose.yml", "Specify an alternate compose `file`")
	flag.StringVar(&outputDir, "output-dir", "output", "Kubernetes configs output `directory`")
	flag.StringVar(&pullPolicy, "pull-policy", "", "Image Pull policy")
	flag.StringVar(&nodeSelector, "node-selector", "", "Node Selector in the format of 'key=value;key2=value2'")
}

func main() {
	flag.Parse()

	p := project.NewProject(&project.Context{
		ProjectName:  "kube",
		ComposeFiles: []string{composeFile},
	})

	if err := p.Parse(); err != nil {
		log.Fatalf("Failed to parse the compose project from %s: %v", composeFile, err)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Failed to create the output directory %s: %v", outputDir, err)
	}

	for name, service := range p.Configs {
		pod := &api.PodSpec{
			Containers: []api.Container{
				{
					Name:  strings.ToLower(name),
					Image: service.Image,
					Args:  service.Command.Slice(),
					Resources: api.ResourceRequirements{
						Limits: api.ResourceList{},
					},
				},
			},
		}

		if service.CPUShares != 0 {
			pod.Containers[0].Resources.Limits[api.ResourceCPU] = *resource.NewMilliQuantity(service.CPUShares, resource.BinarySI)
		}

		if service.MemLimit != 0 {
			pod.Containers[0].Resources.Limits[api.ResourceMemory] = *resource.NewQuantity(service.MemLimit, "decimalSI")
		}

		// If Privileged, create a SecurityContext and configure it
		if service.Privileged == true {
			priv := true
			context := &api.SecurityContext{
				Capabilities: &api.Capabilities{},
				Privileged:   &priv,
			}
			pod.Containers[0].SecurityContext = context
		}

		if pullPolicy != "" {
			switch pullPolicy {
			case "", "IfNotPresent":
				pod.Containers[0].ImagePullPolicy = api.PullIfNotPresent
			case "Always":
				pod.Containers[0].ImagePullPolicy = api.PullAlways
			case "Never":
				pod.Containers[0].ImagePullPolicy = api.PullNever
			default:
				log.Fatalf("Unknown pull policy %s for service %s", pullPolicy, name)
			}
		}

		if nodeSelector != "" {
			ss := strings.Split(nodeSelector, ";")
			m := make(map[string]string)
			for _, pair := range ss {
				z := strings.Split(pair, "=")
				m[z[0]] = z[1]
			}
			pod.NodeSelector = m
		}

		// Configure the environment variables
		var environment []api.EnvVar
		for _, envs := range service.Environment.Slice() {
			value := strings.Split(envs, "=")
			environment = append(environment, api.EnvVar{Name: value[0], Value: value[1]})
		}

		pod.Containers[0].Env = environment

		// Configure the container ports.
		var ports []api.ContainerPort
		for _, port := range service.Ports {
			// Check if we have to deal with a mapped port
			if strings.Contains(port, ":") {
				parts := strings.Split(port, ":")
				port = parts[1]
			}
			portNumber, err := strconv.ParseInt(port, 10, 32)
			if err != nil {
				log.Fatalf("Invalid container port %s for service %s", port, name)
			}
			ports = append(ports, api.ContainerPort{ContainerPort: int32(portNumber)})
		}

		pod.Containers[0].Ports = ports

		// Configure the container restart policy.
		var (
			objType string
			data    []byte
			err     error
		)
		switch service.Restart {
		case "", "always":
			objType = "rc"
			pod.RestartPolicy = api.RestartPolicyAlways
			data, err = yaml.Marshal(replicationController(name, pod))
		case "no", "false":
			objType = "pod"
			pod.RestartPolicy = api.RestartPolicyNever
			data, err = yaml.Marshal(job(name, pod))
		case "on-failure":
			objType = "job"
			pod.RestartPolicy = api.RestartPolicyOnFailure
			data, err = yaml.Marshal(job(name, pod))
		default:
			log.Fatalf("Unknown restart policy %s for service %s", service.Restart, name)
		}

		if err != nil {
			log.Fatalf("Failed to marshal: %v", err)
		}

		// Save the job controller for the Docker compose service to the
		// configs directory.
		outputFileName := fmt.Sprintf("%s-%s.yaml", name, objType)
		outputFilePath := filepath.Join(outputDir, outputFileName)
		if err := ioutil.WriteFile(outputFilePath, data, 0644); err != nil {
			log.Fatalf("Failed to write job controller %s: %v", outputFileName, err)
		}
		fmt.Println(outputFilePath)
	}
}

func job(name string, pod *api.PodSpec) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Job",
			APIVersion: "batch/v1",
		},
		ObjectMeta: api.ObjectMeta{
			Name: strings.ToLower(name),
		},
		Spec: batchv1.JobSpec{
			Template: api.PodTemplateSpec{
				ObjectMeta: api.ObjectMeta{
					Labels: map[string]string{"service": name},
				},
				Spec: *pod,
			},
		},
	}
}

func replicationController(name string, pod *api.PodSpec) *api.ReplicationController {
	var replicas int32 = 1
	return &api.ReplicationController{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "ReplicationController",
			APIVersion: "v1",
		},
		ObjectMeta: api.ObjectMeta{
			Name: strings.ToLower(name),
		},
		Spec: api.ReplicationControllerSpec{
			Replicas: &replicas,
			Template: &api.PodTemplateSpec{
				ObjectMeta: api.ObjectMeta{
					Labels: map[string]string{"service": name},
				},
				Spec: *pod,
			},
		},
	}
}

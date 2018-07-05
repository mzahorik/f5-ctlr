/*
Copyright 2016 The Kubernetes Authors.

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

// Note: the example only works with the code within the same release/branch.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"strconv"

	"github.com/scottdware/go-bigip"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	log "github.com/sirupsen/logrus"
)

var clientset *kubernetes.Clientset

var globalConfig struct {
	Partition string
}

type LTMState struct {
	Virtuals []bigip.VirtualServer `json:"virtuals",omitempty`
	Pools    []bigip.Pool          `json:"pools",omitempty`
	Monitors []bigip.Monitor       `json:"monitors",omitempty`
}

type MyJSONFormatter struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Time  string `json:"time"`
	File  string `json:"file"`
	Line  int    `json:"line"`
}

func (f *MyJSONFormatter) Format(entry *log.Entry) ([]byte, error) {

	entry.Time = entry.Time.UTC()
	logrusJF := &(log.JSONFormatter{})
	bytes, _ := logrusJF.Format(entry)

	myF := MyJSONFormatter{}
	json.Unmarshal(bytes, &myF)
	_, file, no, _ := runtime.Caller(7)
	myF.File = file
	myF.Line = no

	jsonStr, err := json.Marshal(myF)
	if err == nil {
		jsonStr = append( jsonStr, '\n')
	}
	return jsonStr, err
}

// Initialize the connection to Kubernetes

func initKubernetes() error {

	// Try initializing using the credentials available to a pod in Kubernetes

	config, err := rest.InClusterConfig()
	if err == nil {
		log.Debugf("Connected using in-pod credentials")
		clientset, err = kubernetes.NewForConfig(config)
		return err
	}

	// That didn't work, try initializing using $HOME/.kube/config

	var home string
	if home = os.Getenv("HOME"); home == "" {
		return fmt.Errorf("HOME environment variable must be set")
	}

	kubeconfigFile := flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "absolute path to the kubeconfig file")
	flag.Parse()
	config, err = clientcmd.BuildConfigFromFlags("", *kubeconfigFile)
	if err == nil {
		clientset, err = kubernetes.NewForConfig(config)
		return err
	}

	// That didn't work, return with whatever error fell through to here

	return err
}

func buildCurrentLTMState() (LTMState, error) {

	var cs LTMState

	var f5_user, f5_password, f5_host string

	if f5_user = os.Getenv("F5_USER"); f5_user == "" {
		return cs, fmt.Errorf("F5_USER environment variable must be set")
	}

	if f5_password = os.Getenv("F5_PASSWORD"); f5_password == "" {
		return cs, fmt.Errorf("F5_PASSWORD environment variable must be set")
	}

	if f5_host = os.Getenv("F5_HOST"); f5_host == "" {
		return cs, fmt.Errorf("F5_HOST environment variable must be set")
	}

	f5, err := bigip.NewTokenSession(f5_host, f5_user, f5_password, "tmos", &bigip.ConfigOptions{})

	if err != nil {
		log.Debugf("Failed to get token")
		return cs, err
	}

	log.Debugf("Connected to F5")

	pools, err := f5.Pools()
	if err != nil {
		log.Debugf("Failed to retrieve pool information")
		return cs, err
	}

	log.Debugf("Successfully fetched pools")
	for _, pool := range pools.Pools {
		log.Debugf("Found a pool")
		if pool.Partition == globalConfig.Partition {
			log.Debugf("Pool matches partition name")
			cs.Pools = append( cs.Pools, pool)
		}
	}

	return cs, nil
}

type KsVSMonitorAttributes struct {
	Interval int    `json:"interval",omitempty`
	Send     string `json:"send",omitempty`
	Receive  string `json:"recv",omitempty`
	Timeout  int    `json:"timeout",omitempty`
	Type     string `json:"type",omitempty`
}

type KsVSMember struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
	IP   string `json:"ip"`
}

type KsVirtualServer struct {
	Name         string                `json:"name"`
	Namespace    string                `json:"namespace"`
	IP           string                `json:"ip"`
	Port         int32                 `json:"port"`
	ClientSSL    string                `json:"clientssl",omitempty`
	ServerSSL    string                `json:"serverssl",omitempty`
	Redirect     bool                  `json:"redirect",omitempty`
	DefPersist   string                `json:"persist",omitempty`
	FBPersist    string                `json:"fallbackPersist",omitempty`
	LBMode       string                `json:"lbmode",omitempty`
	IRules       []string              `json:"rules",omitempty`
	Members      []KsVSMember          `json:"members",omitempty`
	Monitor      KsVSMonitorAttributes `json:"monitors",omitempty`
}

type KubernetesState []KsVirtualServer

func getKubernetesState() (KubernetesState, error) {

	var ks KubernetesState

	ingresses, err := clientset.ExtensionsV1beta1().Ingresses("").List(metav1.ListOptions{})
	if err != nil {
		return ks, err
	}
	log.Debugf("Successfully fetched all Ingress objects from Kubernetes")

	services, err := clientset.CoreV1().Services("").List(metav1.ListOptions{})
	if err != nil {
		return ks, err
	}
	log.Debugf("Successfully fetched all Service objects from Kubernetes")

	// Loop through the Ingress objects, building complete virtual server objects

	for _, ingress := range ingresses.Items {

		// Set basic parameters of the virtual server

		var vs KsVirtualServer

		vs.Name = ingress.GetName()
		vs.Namespace = ingress.GetNamespace()

		if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/ip"]; ok == true {
			if ip := net.ParseIP(value); ip != nil {
				vs.IP = value
			} else {
				log.WithFields(log.Fields{
				  "ingress": vs.Name,
				  "namespace": vs.Namespace,
				  "ip": value,
				}).Errorf("Invalid IP address for ip annotation")
			}
		} else {
			log.WithFields(log.Fields{
			  "ingress": vs.Name,
			  "namespace": vs.Namespace,
			}).Infof("No IP address, creating headless virtual server")
		}

		if len(ingress.Spec.TLS) != 0 {
			vs.ClientSSL = ingress.Spec.TLS[0].SecretName
			vs.Redirect = true
			if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/https-port"]; ok == true {
				port, _ := strconv.ParseInt(value, 10, 32)
				vs.Port = int32(port)
			} else {
				vs.Port = 443
			}
			if value, ok := ingress.ObjectMeta.Annotations["ingress.kubernetes.io/ssl-redirect"]; ok == true {
				if value == "false" {
					vs.Redirect = false
				}
			}
		} else {
			if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/http-port"]; ok == true {
				port, _ := strconv.ParseInt(value, 10, 32)
				vs.Port = int32(port)
			} else {
				vs.Port = 80
			}
		}

		if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/health"]; ok == true {
			var monitors []KsVSMonitorAttributes

			err := json.Unmarshal([]byte(value), &monitors)
			if err != nil {
				log.Debugf("health monitor JSON parsing failed")
			}
			vs.Monitor = monitors[0]
		}

		if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/serverssl"]; ok == true {
			vs.ServerSSL = value
			if vs.Monitor.Type == "" {
				vs.Monitor.Type = "https"
			}
		}

		if vs.Monitor.Type == "" {
			vs.Monitor.Type = "http"
		}

		if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/rules"]; ok == true {
			parts := strings.Split(value, ",")
			for idx := range parts {
				vs.IRules = append( vs.IRules, parts[idx])
			}
		}

		if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/balance"]; ok == true {
			vs.LBMode = value
		}

		if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/defaultPersist"]; ok == true {
			vs.DefPersist = value
		}

		if value, ok := ingress.ObjectMeta.Annotations["virtual-server.f5.com/fallbackPersist"]; ok == true {
			vs.FBPersist = value
		}

		// Find a matching service

		var service v1.Service

		err = fmt.Errorf("Not found")
		for _, service = range services.Items {
			if service.GetName() == ingress.Spec.Backend.ServiceName && service.GetNamespace() == vs.Namespace {
				err = nil
				break
			}
		}
		if err != nil {
			log.WithFields(log.Fields{
			  "ingress": vs.Name,
			  "namespace": vs.Namespace,
			  "service": service.GetName(),
			}).Infof("Service not found, skipping this Ingress")
			continue
		}

		// Build an array of pods and attach it to the virtual server as members
		// Proceed (with a warning to user) if there are no pods
		// Proceed (with a warning to user) if a pod is not running (there is no IP for non-runnig pods)

		set := labels.Set(service.Spec.Selector).String()

		pods, err := clientset.Core().Pods(vs.Namespace).List(metav1.ListOptions{LabelSelector: set})
		if err == nil {
			for _, pod := range pods.Items {
				var member KsVSMember

				if pod.Status.Phase == "Running" {
					member.Name = pod.GetName()
					member.IP = pod.Status.PodIP
					member.Port = int32(ingress.Spec.Backend.ServicePort.IntValue())
					vs.Members = append( vs.Members, member)
					log.WithFields(log.Fields{
					  "ingress": vs.Name,
					  "namespace": vs.Namespace,
					  "pod": member.Name,
					  "ip": member.IP,
					  "port": member.Port,
					}).Debugf("Adding pod to virtual server")
			} else {
					log.WithFields(log.Fields{
					  "ingress": vs.Name,
					  "namespace": vs.Namespace,
					  "pod": pod.GetName(),
					}).Infof("Skipping pod that is not running")
				}
			}
		} else {
			log.WithFields(log.Fields{
			  "ingress": vs.Name,
			  "namespace": vs.Namespace,
			}).Debugf("Call to fetch pods failed")
			log.Debugf(err.Error())
		}

		if vs.Members == nil {
			log.WithFields(log.Fields{
			  "ingress": vs.Name,
			  "namespace": vs.Namespace,
			}).Debugf("No pods found, creating empty Ingress")
		}

		// Attach the new virtual server to the slice

		ks = append( ks, vs )
	}

	return ks, nil
}

func main() {

//	log.SetFormatter(&MyJSONFormatter{})

	// initialize the Kubernetes connection

	log.SetLevel(log.DebugLevel)

	err := initKubernetes()
	if err != nil || clientset == nil {
		log.Error("Could not initialize a connection to Kubernetes")
		log.Error(err.Error())
		os.Exit(1)
	}

	globalConfig.Partition = "k8s-auto-ny2"

	desiredState, err := getKubernetesState()
	if err != nil {
		log.Error("Could not fetch desired state from Kubernetes")
		log.Error(err.Error())
		os.Exit(1)
	}

	desiredJson, _ := json.MarshalIndent(desiredState,"", "  ")
	fmt.Printf(string(desiredJson))

	currentState, err := buildCurrentLTMState()
	if err != nil {
		log.Error("Could not fetch current state from F5")
		log.Error(err.Error())
		os.Exit(1)
	}

	currentJson, _ := json.MarshalIndent(currentState,"", "  ")
	fmt.Printf(string(currentJson))
}

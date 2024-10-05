package main

import (
	"context"
	"fmt"
	"log"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	"k8s.io/kubectl/pkg/drain"
)

const (
	highCPULimit = 95.0
	duration     = 10 * time.Minute
)

func main() {
	// Load kubeconfig and create clients
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	config, err := kubeconfig.ClientConfig()
	if err != nil {
		log.Fatalf("Error loading kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating kubernetes client: %v", err)
	}

	metricsClient, err := metricsv.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating metrics client: %v", err)
	}

	// Periodically check node CPU usage
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Printf("Error listing nodes: %v", err)
			continue
		}

		for _, node := range nodes.Items {
			cpuUsage, err := getNodeCPUUsage(metricsClient, node.Name)
			if err != nil {
				log.Printf("Error getting CPU usage for node %s: %v", node.Name, err)
				continue
			}

			if cpuUsage > highCPULimit {
				log.Printf("Node %s has high CPU usage: %.2f%%", node.Name, cpuUsage)
				err := cordonAndDrainNode(clientset, &node)
				if err != nil {
					log.Printf("Error cordoning and draining node %s: %v", node.Name, err)
				}
			}
		}
	}
}

func getNodeCPUUsage(metricsClient *metricsv.Clientset, nodeName string) (float64, error) {
	nodeMetrics, err := metricsClient.MetricsV1beta1().NodeMetricses().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}

	cpuUsage := nodeMetrics.Usage[v1.ResourceCPU]
	cpuMilliValue := cpuUsage.MilliValue()
	// Assuming a 1 CPU core node
	totalCPU := float64(1000) // 1000 millicores = 1 CPU

	usagePercent := (float64(cpuMilliValue) / totalCPU) * 100
	return usagePercent, nil
}

func cordonAndDrainNode(clientset *kubernetes.Clientset, node *v1.Node) error {
	drainer := &drain.Helper{
		Ctx:                 context.TODO(),
		Client:              clientset,
		Force:               true,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		Timeout:             5 * time.Minute,
		DeleteEmptyDirData:  true,
	}

	// Cordon the node
	err := drain.RunCordonOrUncordon(drainer, node, true)
	if err != nil {
		return fmt.Errorf("error cordoning node: %v", err)
	}
	log.Printf("Node %s cordoned", node.Name)

	// Drain the node
	err = drain.RunNodeDrain(drainer, node.Name)
	if err != nil {
		return fmt.Errorf("error draining node: %v", err)
	}
	log.Printf("Node %s drained", node.Name)

	return nil
}


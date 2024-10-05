// main.go
package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

const (
	checkInterval  = 1 * time.Minute
	thresholdUsage = 95.0
	thresholdTime  = 10 * time.Minute
)

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	metricsClientset, err := versioned.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	for {
		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("Error listing nodes: %v\n", err)
			time.Sleep(checkInterval)
			continue
		}

		for _, node := range nodes.Items {
			nodeName := node.Name
			usage, err := getNodeCPUUsage(metricsClientset, nodeName)
			if err != nil {
				fmt.Printf("Error getting CPU usage for node %s: %v\n", nodeName, err)
				continue
			}

			fmt.Printf("Node: %s, CPU Usage: %.2f%%\n", nodeName, usage)

			if usage > thresholdUsage {
				if shouldDrainAndCordon(clientset, nodeName) {
					if err := drainAndCordonNode(clientset, nodeName); err != nil {
						fmt.Printf("Error draining and cordoning node %s: %v\n", nodeName, err)
					} else {
						fmt.Printf("Node %s has been drained and cordoned\n", nodeName)
					}
				}
			}
		}

		time.Sleep(checkInterval)
	}
}

func getNodeCPUUsage(metricsClientset *versioned.Clientset, nodeName string) (float64, error) {
	nodeMetrics, err := metricsClientset.MetricsV1beta1().NodeMetricses().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}

	cpuQuantity := nodeMetrics.Usage.Cpu()
	return float64(cpuQuantity.MilliValue()) / 10, nil
}

func shouldDrainAndCordon(clientset *kubernetes.Clientset, nodeName string) bool {
	events, err := clientset.CoreV1().Events("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.kind=Node,involvedObject.name=%s,reason=NodeHighCPUUsage", nodeName),
	})
	if err != nil {
		fmt.Printf("Error getting events for node %s: %v\n", nodeName, err)
		return false
	}

	if len(events.Items) == 0 {
		createHighCPUEvent(clientset, nodeName)
		return false
	}

	lastEvent := events.Items[len(events.Items)-1]
	if time.Since(lastEvent.LastTimestamp.Time) >= thresholdTime {
		return true
	}

	return false
}

func createHighCPUEvent(clientset *kubernetes.Clientset, nodeName string) {
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "high-cpu-usage-",
			Namespace:    "",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
		Reason:  "NodeHighCPUUsage",
		Message: "Node CPU usage is above 95%",
		Type:    "Warning",
	}

	_, err := clientset.CoreV1().Events("").Create(context.TODO(), event, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Error creating high CPU event for node %s: %v\n", nodeName, err)
	}
}

func drainAndCordonNode(clientset *kubernetes.Clientset, nodeName string) error {
	// Cordon the node
	_, err := clientset.CoreV1().Nodes().Patch(context.TODO(), nodeName, types.StrategicMergePatchType,
		[]byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("error cordoning node: %v", err)
	}

	// Drain the node
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return fmt.Errorf("error listing pods on node: %v", err)
	}

	for _, pod := range pods.Items {
		err := clientset.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		if err != nil {
			fmt.Printf("Error deleting pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
		}
	}

	return nil
}

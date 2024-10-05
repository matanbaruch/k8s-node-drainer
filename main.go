package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

var (
	checkInterval  time.Duration
	thresholdUsage float64
	thresholdTime  time.Duration
	namespace      string

	highCPUNodes      = make(map[string]time.Time)
	highCPUNodesMutex sync.Mutex
)

func init() {
	namespace = getEnv("POD_NAMESPACE", "default")
	checkInterval = getDurationEnv("CHECK_INTERVAL", 1*time.Minute)
	thresholdUsage = getFloatEnv("THRESHOLD_USAGE", 95.0)
	thresholdTime = getDurationEnv("THRESHOLD_TIME", 10*time.Minute)

	fmt.Printf("Configuration:\n")
	fmt.Printf("  Namespace: %s\n", namespace)
	fmt.Printf("  Check Interval: %v\n", checkInterval)
	fmt.Printf("  Threshold Usage: %.2f%%\n", thresholdUsage)
	fmt.Printf("  Threshold Time: %v\n", thresholdTime)
}

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

			if node.Spec.Unschedulable {
				fmt.Printf("Node: %s is already cordoned, skipping\n", nodeName)
				continue
			}

			usage, err := getNodeCPUUsage(metricsClientset, nodeName)
			if err != nil {
				fmt.Printf("Error getting CPU usage for node %s: %v\n", nodeName, err)
				continue
			}

			highCPUDuration := getHighCPUDuration(nodeName)
			fmt.Printf("Node: %s, CPU Usage: %.2f%%, High CPU Duration: %s\n", nodeName, usage, highCPUDuration)

			if usage > thresholdUsage {
				if shouldDrainAndCordon(nodeName) {
					if err := drainAndCordonNode(clientset, nodeName); err != nil {
						fmt.Printf("Error draining and cordoning node %s: %v\n", nodeName, err)
					} else {
						fmt.Printf("Node %s has been drained and cordoned\n", nodeName)
						removeHighCPUNode(nodeName)
						createNodeDrainedEvent(clientset, nodeName, highCPUDuration)
					}
				} else {
					createHighCPUEvent(clientset, nodeName, highCPUDuration)
				}
			} else {
				removeHighCPUNode(nodeName)
			}
		}

		time.Sleep(checkInterval)
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getFloatEnv(key string, fallback float64) float64 {
	strValue := getEnv(key, "")
	if strValue == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(strValue, 64)
	if err != nil {
		fmt.Printf("Error parsing %s, using fallback value: %v\n", key, err)
		return fallback
	}
	return value
}

func getDurationEnv(key string, fallback time.Duration) time.Duration {
	strValue := getEnv(key, "")
	if strValue == "" {
		return fallback
	}
	value, err := time.ParseDuration(strValue)
	if err != nil {
		fmt.Printf("Error parsing %s, using fallback value: %v\n", key, err)
		return fallback
	}
	return value
}

func getNodeCPUUsage(metricsClientset *versioned.Clientset, nodeName string) (float64, error) {
	nodeMetrics, err := metricsClientset.MetricsV1beta1().NodeMetricses().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}

	cpuQuantity := nodeMetrics.Usage.Cpu()
	return float64(cpuQuantity.MilliValue()) / 10, nil
}

func getHighCPUDuration(nodeName string) time.Duration {
	highCPUNodesMutex.Lock()
	defer highCPUNodesMutex.Unlock()

	firstSeen, exists := highCPUNodes[nodeName]
	if !exists {
		return 0
	}

	return time.Since(firstSeen)
}

func shouldDrainAndCordon(nodeName string) bool {
	highCPUNodesMutex.Lock()
	defer highCPUNodesMutex.Unlock()

	firstSeen, exists := highCPUNodes[nodeName]
	if !exists {
		highCPUNodes[nodeName] = time.Now()
		return false
	}

	return time.Since(firstSeen) >= thresholdTime
}

func removeHighCPUNode(nodeName string) {
	highCPUNodesMutex.Lock()
	defer highCPUNodesMutex.Unlock()

	delete(highCPUNodes, nodeName)
}

func drainAndCordonNode(clientset *kubernetes.Clientset, nodeName string) error {
	// Cordon the node
	node, err := clientset.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting node: %v", err)
	}

	if node.Spec.Unschedulable {
		fmt.Printf("Node %s is already cordoned\n", nodeName)
		return nil
	}

	_, err = clientset.CoreV1().Nodes().Patch(context.TODO(), nodeName, types.StrategicMergePatchType,
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

func createHighCPUEvent(clientset *kubernetes.Clientset, nodeName string, duration time.Duration) {
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "high-cpu-usage-",
			Namespace:    namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
		Reason:  "NodeHighCPUUsage",
		Message: fmt.Sprintf("Node %s CPU usage is above %.2f%% for %s", nodeName, thresholdUsage, duration.Round(time.Second)),
		Type:    "Warning",
	}

	_, err := clientset.CoreV1().Events(namespace).Create(context.TODO(), event, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Error creating high CPU event for node %s: %v\n", nodeName, err)
	}
}

func createNodeDrainedEvent(clientset *kubernetes.Clientset, nodeName string, duration time.Duration) {
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "node-drained-",
			Namespace:    namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
		Reason:  "NodeDrained",
		Message: fmt.Sprintf("Node %s has been drained and cordoned due to high CPU usage (above %.2f%%) for %s", nodeName, thresholdUsage, duration.Round(time.Second)),
		Type:    "Warning",
	}

	_, err := clientset.CoreV1().Events(namespace).Create(context.TODO(), event, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Error creating node drained event for node %s: %v\n", nodeName, err)
	}
}

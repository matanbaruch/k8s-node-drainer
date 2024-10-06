package main

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
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

	log *logrus.Logger
)

func init() {
	// Initialize logger
	log = logrus.New()
	log.SetFormatter(&logrus.JSONFormatter{})
	log.SetOutput(os.Stdout)

	namespace = getEnv("POD_NAMESPACE", "default")
	checkInterval = getDurationEnv("CHECK_INTERVAL", 1*time.Minute)
	thresholdUsage = getFloatEnv("THRESHOLD_USAGE", 95.0)
	thresholdTime = getDurationEnv("THRESHOLD_TIME", 10*time.Minute)

	log.WithFields(logrus.Fields{
		"namespace":       namespace,
		"checkInterval":   checkInterval,
		"thresholdUsage":  thresholdUsage,
		"thresholdTime":   thresholdTime,
	}).Info("Configuration loaded")
}

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.WithError(err).Fatal("Failed to get in-cluster config")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.WithError(err).Fatal("Failed to create Kubernetes clientset")
	}

	metricsClientset, err := versioned.NewForConfig(config)
	if err != nil {
		log.WithError(err).Fatal("Failed to create Metrics clientset")
	}

	for {
		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.WithError(err).Error("Error listing nodes")
			time.Sleep(checkInterval)
			continue
		}

		for _, node := range nodes.Items {
			nodeName := node.Name

			if node.Spec.Unschedulable {
				log.WithField("node", nodeName).Info("Node is already cordoned, skipping")
				continue
			}

			usage, err := getNodeCPUUsage(metricsClientset, nodeName)
			if err != nil {
				log.WithError(err).WithField("node", nodeName).Error("Error getting CPU usage")
				continue
			}

			highCPUDuration := getHighCPUDuration(nodeName)
			log.WithFields(logrus.Fields{
				"node":             nodeName,
				"cpuUsage":         usage,
				"highCPUDuration":  highCPUDuration,
			}).Info("Node CPU usage")

			if usage > thresholdUsage {
				if shouldDrainAndCordon(nodeName) {
					if err := drainAndCordonNode(clientset, nodeName); err != nil {
						log.WithError(err).WithField("node", nodeName).Error("Error draining and cordoning node")
					} else {
						log.WithField("node", nodeName).Info("Node has been drained and cordoned")
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
		log.WithError(err).WithFields(logrus.Fields{
			"key":      key,
			"fallback": fallback,
		}).Error("Error parsing float environment variable")
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
		log.WithError(err).WithFields(logrus.Fields{
			"key":      key,
			"fallback": fallback,
		}).Error("Error parsing duration environment variable")
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
		return err
	}

	if node.Spec.Unschedulable {
		log.WithField("node", nodeName).Info("Node is already cordoned")
		return nil
	}

	_, err = clientset.CoreV1().Nodes().Patch(context.TODO(), nodeName, types.StrategicMergePatchType,
		[]byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{})
	if err != nil {
		return err
	}

	// Drain the node
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return err
	}

	for _, pod := range pods.Items {
		err := clientset.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		if err != nil {
			log.WithError(err).WithFields(logrus.Fields{
				"node":      nodeName,
				"namespace": pod.Namespace,
				"pod":       pod.Name,
			}).Error("Error deleting pod")
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
		Message: "Node CPU usage is above threshold",
		Type:    "Warning",
	}

	_, err := clientset.CoreV1().Events(namespace).Create(context.TODO(), event, metav1.CreateOptions{})
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			"node":     nodeName,
			"duration": duration,
		}).Error("Error creating high CPU event")
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
		Message: "Node has been drained and cordoned due to high CPU usage",
		Type:    "Warning",
	}

	_, err := clientset.CoreV1().Events(namespace).Create(context.TODO(), event, metav1.CreateOptions{})
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			"node":     nodeName,
			"duration": duration,
		}).Error("Error creating node drained event")
	}
}

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
	checkInterval       time.Duration
	thresholdUtilization float64
	thresholdTime       time.Duration
	namespace           string
	dryRun              bool

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
	thresholdUtilization = getFloatEnv("THRESHOLD_UTILIZATION", 95.0)
	thresholdTime = getDurationEnv("THRESHOLD_TIME", 10*time.Minute)
	dryRun = getBoolEnv("DRY_RUN", false)

	log.WithFields(logrus.Fields{
		"namespace":            namespace,
		"checkInterval":        checkInterval,
		"thresholdUtilization": thresholdUtilization,
		"thresholdTime":        thresholdTime,
		"dryRun":               dryRun,
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

			utilization, err := getNodeCPUUtilization(metricsClientset, clientset, nodeName)
			if err != nil {
				log.WithError(err).WithField("node", nodeName).Error("Error getting CPU utilization")
				continue
			}

			highCPUDuration := getHighCPUDuration(nodeName)
			log.WithFields(logrus.Fields{
				"node":             nodeName,
				"cpuUtilization":   utilization,
				"highCPUDuration":  highCPUDuration,
			}).Info("Node CPU utilization")

			if utilization > thresholdUtilization {
				if shouldDrainAndCordon(nodeName) {
					if dryRun {
						log.WithField("node", nodeName).Info("DRY RUN: Node would be drained and cordoned")
					} else {
						if err := drainAndCordonNode(clientset, nodeName); err != nil {
							log.WithError(err).WithField("node", nodeName).Error("Error draining and cordoning node")
						} else {
							log.WithField("node", nodeName).Info("Node has been drained and cordoned")
							removeHighCPUNode(nodeName)
							createNodeDrainedEvent(clientset, nodeName, highCPUDuration)
						}
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

func getNodeCPUUtilization(metricsClientset *versioned.Clientset, clientset *kubernetes.Clientset, nodeName string) (float64, error) {
	nodeMetrics, err := metricsClientset.MetricsV1beta1().NodeMetricses().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}

	node, err := clientset.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}

	cpuUsage := nodeMetrics.Usage.Cpu().MilliValue()
	cpuCapacity := node.Status.Capacity.Cpu().MilliValue()

	utilization := (float64(cpuUsage) / float64(cpuCapacity)) * 100
	return utilization, nil
}

// Other functions remain the same...

func createHighCPUEvent(clientset *kubernetes.Clientset, nodeName string, duration time.Duration) {
	if dryRun {
		log.WithFields(logrus.Fields{
			"node":     nodeName,
			"duration": duration,
		}).Info("DRY RUN: Would create high CPU event")
		return
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "high-cpu-utilization-",
			Namespace:    namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
		Reason:  "NodeHighCPUUtilization",
		Message: "Node CPU utilization is above threshold",
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
	if dryRun {
		log.WithFields(logrus.Fields{
			"node":     nodeName,
			"duration": duration,
		}).Info("DRY RUN: Would create node drained event")
		return
	}

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
		Message: "Node has been drained and cordoned due to high CPU utilization",
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

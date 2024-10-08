package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/metrics/pkg/client/clientset/versioned"
)

var (
	checkInterval        time.Duration
	thresholdUtilization float64
	thresholdTime        time.Duration
	namespace            string
	dryRun               bool
	nodeLabelSelector    string

	highCPUNodes      = make(map[string]time.Time)
	highCPUNodesMutex sync.Mutex

	log *logrus.Logger

	// Prometheus metrics
	nodeCPUUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_cpu_utilization",
			Help: "Current CPU utilization of the node",
		},
		[]string{"nodeName"},
	)
	highCPUNodesDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "high_cpu_nodes_duration_seconds",
			Help: "Duration for which a node has been in high CPU state",
		},
		[]string{"nodeName"},
	)
	nodesDrainedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nodes_drained_total",
			Help: "Total number of nodes drained due to high CPU",
		},
		[]string{"nodeName"},
	)
	highCPUEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "high_cpu_events_total",
			Help: "Total number of high CPU events created",
		},
		[]string{"nodeName"},
	)
)

// Function to format the labels as "key=value,key=value"
func formatLabels(labels map[string]string) string {
	var labelPairs []string
	for key, value := range labels {
		labelPairs = append(labelPairs, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(labelPairs, ",")
}

func init() {
	// Initialize logger
	log = logrus.New()
	log.SetFormatter(&logrus.JSONFormatter{
		DisableTimestamp: true,
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyMsg: "message",
		},
	})
	log.SetOutput(os.Stdout)

	namespace = getEnv("POD_NAMESPACE", "default")
	checkInterval = getDurationEnv("CHECK_INTERVAL", 1*time.Minute)
	thresholdUtilization = getFloatEnv("THRESHOLD_UTILIZATION", 95.0)
	thresholdTime = getDurationEnv("THRESHOLD_TIME", 10*time.Minute)
	dryRun = getBoolEnv("DRY_RUN", false)
	nodeLabelSelector = getEnv("NODE_LABEL_SELECTOR", "")

	log.WithFields(logrus.Fields{
		"namespace":            namespace,
		"checkInterval":        checkInterval.String(),
		"thresholdUtilization": fmt.Sprintf("%.2f%%", thresholdUtilization),
		"thresholdTime":        thresholdTime.String(),
		"dryRun":               dryRun,
		"nodeLabelSelector":    nodeLabelSelector,
	}).Info("Configuration loaded")

	// Register Prometheus metrics
	prometheus.MustRegister(nodeCPUUtilization)
	prometheus.MustRegister(highCPUNodesDuration)
	prometheus.MustRegister(nodesDrainedTotal)
	prometheus.MustRegister(highCPUEventsTotal)
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

	// Start Prometheus HTTP server
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Info("Starting Prometheus metrics server on :8080")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.WithError(err).Fatal("Failed to start Prometheus metrics server")
		}
	}()

	for {
		listOptions := metav1.ListOptions{}
		if nodeLabelSelector != "" {
			listOptions.LabelSelector = nodeLabelSelector
		}

		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), listOptions)
		if err != nil {
			log.WithError(err).Error("Error listing nodes")
			time.Sleep(checkInterval)
			continue
		}

		for _, node := range nodes.Items {
			nodeName := node.Name
			nodeLabels := formatLabels(node.Labels)

			if node.Spec.Unschedulable {
				log.WithFields(logrus.Fields{
					"nodeName": nodeName,
					"labels":   nodeLabels,
				}).Info("Node is already cordoned, skipping")
				continue
			}

			utilization, err := getNodeCPUUtilization(metricsClientset, clientset, nodeName)
			if err != nil {
				log.WithError(err).WithFields(logrus.Fields{
					"nodeName": nodeName,
					"labels":   nodeLabels,
				}).Error("Error getting CPU utilization")
				continue
			}

			// Update Prometheus metric
			nodeCPUUtilization.WithLabelValues(nodeName).Set(utilization)

			highCPUDuration := getHighCPUDuration(nodeName)
			log.WithFields(logrus.Fields{
				"nodeName":        nodeName,
				"labels":          nodeLabels,
				"cpuUtilization":  fmt.Sprintf("%.2f%%", utilization),
				"highCPUDuration": highCPUDuration.String(),
			}).Info("Node CPU utilization")

			// Update Prometheus metric
			highCPUNodesDuration.WithLabelValues(nodeName).Set(highCPUDuration.Seconds())

			if utilization > thresholdUtilization {
				if shouldDrainAndCordon(nodeName) {
					if dryRun {
						log.WithFields(logrus.Fields{
							"nodeName": nodeName,
							"labels":   nodeLabels,
						}).Info("DRY RUN: Node would be drained and cordoned")
					} else {
						if err := drainAndCordonNode(clientset, nodeName); err != nil {
							log.WithError(err).WithFields(logrus.Fields{
								"nodeName": nodeName,
								"labels":   nodeLabels,
							}).Error("Error draining and cordoning node")
						} else {
							log.WithFields(logrus.Fields{
								"nodeName": nodeName,
								"labels":   nodeLabels,
							}).Info("Node has been drained and cordoned")
							removeHighCPUNode(nodeName)
							createNodeDrainedEvent(clientset, nodeName, highCPUDuration, node.Labels)
							// Update Prometheus metric
							nodesDrainedTotal.WithLabelValues(nodeName).Inc()
						}
					}
				} else {
					createHighCPUEvent(clientset, nodeName, highCPUDuration, node.Labels)
					// Update Prometheus metric
					highCPUEventsTotal.WithLabelValues(nodeName).Inc()
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

func getBoolEnv(key string, fallback bool) bool {
	strValue := getEnv(key, "")
	if strValue == "" {
		return fallback
	}
	value, err := strconv.ParseBool(strValue)
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			"key":      key,
			"fallback": fallback,
		}).Error("Error parsing boolean environment variable")
		return fallback
	}
	return value
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
		log.WithField("nodeName", nodeName).Info("Node is already cordoned")
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

	log.WithFields(logrus.Fields{
		"nodeName": nodeName,
		"podCount": len(pods.Items),
	}).Info("Starting to drain node")

	for _, pod := range pods.Items {
		err := clientset.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
		if err != nil {
			log.WithError(err).WithFields(logrus.Fields{
				"nodeName":  nodeName,
				"namespace": pod.Namespace,
				"pod":       pod.Name,
			}).Error("Error deleting pod")
		} else {
			log.WithFields(logrus.Fields{
				"nodeName":  nodeName,
				"namespace": pod.Namespace,
				"pod":       pod.Name,
			}).Info("Successfully deleted pod")
		}
	}

	log.WithField("nodeName", nodeName).Info("Node draining completed")
	return nil
}

func getNodeLabelsString(labels map[string]string) string {
	result := ""
	for k, v := range labels {
		if result != "" {
			result += ","
		}
		result += fmt.Sprintf("%s=%s", k, v)
	}
	return result
}

func createHighCPUEvent(clientset *kubernetes.Clientset, nodeName string, duration time.Duration, nodeLabels map[string]string) {
	if dryRun {
		log.WithFields(logrus.Fields{
			"nodeName": nodeName,
			"labels":   formatLabels(nodeLabels),
			"duration": duration.String(),
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
		Message: fmt.Sprintf("Node CPU utilization is above threshold for %s. Node labels: %s", duration.String(), formatLabels(nodeLabels)),
		Type:    "Warning",
	}

	_, err := clientset.CoreV1().Events(namespace).Create(context.TODO(), event, metav1.CreateOptions{})
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			"nodeName": nodeName,
			"labels":   formatLabels(nodeLabels),
			"duration": duration.String(),
		}).Error("Error creating high CPU event")
	} else {
		log.WithFields(logrus.Fields{
			"nodeName": nodeName,
			"labels":   formatLabels(nodeLabels),
			"duration": duration.String(),
		}).Info("Successfully created high CPU event")
	}
}

func createNodeDrainedEvent(clientset *kubernetes.Clientset, nodeName string, duration time.Duration, nodeLabels map[string]string) {
	if dryRun {
		log.WithFields(logrus.Fields{
			"nodeName": nodeName,
			"labels":   formatLabels(nodeLabels),
			"duration": duration.String(),
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
		Message: fmt.Sprintf("Node has been drained and cordoned due to high CPU utilization for %s. Node labels: %s", duration.String(), formatLabels(nodeLabels)),
		Type:    "Warning",
	}

	_, err := clientset.CoreV1().Events(namespace).Create(context.TODO(), event, metav1.CreateOptions{})
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			"nodeName": nodeName,
			"labels":   formatLabels(nodeLabels),
			"duration": duration.String(),
		}).Error("Error creating node drained event")
	} else {
		log.WithFields(logrus.Fields{
			"nodeName": nodeName,
			"labels":   formatLabels(nodeLabels),
			"duration": duration.String(),
		}).Info("Successfully created node drained event")
	}
}

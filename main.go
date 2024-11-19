package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	logger    *slog.Logger
	clientset *kubernetes.Clientset
)

func init() {
	var debug bool
	flag.BoolVar(&debug, "debug", true, "debug mode")
	flag.Parse()

	levelInfo := slog.LevelInfo
	if debug {
		levelInfo = slog.LevelDebug
	}

	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: levelInfo,
	}))

	slog.SetDefault(logger)

	logger.Info("starting peer-resolver")
}

func main() {
	defer func() {
		logger.Info("shutting down peer-resolver")
	}()

	var namespace, app, nodesFile, kubeConfig string
	var peerPort, apiPort int

	flag.StringVar(&kubeConfig, "kubeconfig", "config", "kubeconfig file in ~/.kube to work with")
	flag.StringVar(&namespace, "namespace", "typesense", "namespace that typesense is installed within")
	flag.StringVar(&app, "app", "typesense-sts", "name of the typesense app to watch the pods of")
	flag.StringVar(&nodesFile, "nodes-file", "/usr/share/typesense/nodes", "location of the file to write node information to")
	flag.IntVar(&peerPort, "peer-port", 8107, "port on which typesense peering service listens")
	flag.IntVar(&apiPort, "api-port", 8108, "port on which typesense API service listens")

	flag.Parse()

	configPath := filepath.Join(homedir.HomeDir(), ".kube", kubeConfig)
	var config *rest.Config
	var err error

	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		// No config file found, fall back to in-cluster config.
		config, err = rest.InClusterConfig()
		if err != nil {
			logger.Error(fmt.Sprintf("failed to build local config: %s", err))
			os.Exit(1)
		}
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			logger.Error(fmt.Sprintf("failed to build in-cluster config: %s", err))
			os.Exit(1)
		}
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create kubernetes client: %s", err))
		os.Exit(1)
	}

	logger.Info("creating pods watcher")
	watcher, err := clientset.CoreV1().Endpoints(namespace).Watch(context.Background(), metav1.ListOptions{})
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create pods watcher: %s", err))
		os.Exit(1)
	}

	logger.Info("watching for pods", "namespace", namespace, "app", app, "peerPort", peerPort, "apiPort", apiPort)

	for range watcher.ResultChan() {
		err := os.WriteFile(nodesFile, []byte(getNodes(namespace, app, peerPort, apiPort)), 0666)
		if err != nil {
			logger.Error(fmt.Sprintf("failed to write nodes file: %s", err))
		}
	}
}

func getNodes(namespace, app string, peerPort int, apiPort int) string {
	var nodes []string

	listOptions := metav1.ListOptions{
		LabelSelector: "app=" + app,
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), listOptions)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to list pods: %s", err))
		return ""
	}

	if len(pods.Items) == 0 {
		logger.Debug(fmt.Sprintf("no pods found for app: %s/%s", namespace, app))
	}

	for _, pod := range pods.Items {
		for containerIdx, container := range pod.Spec.Containers {
			if container.Name != "typesense" {
				continue
			}

			for _, port := range container.Ports {
				if int(port.ContainerPort) == apiPort {
					ready := false
					if containerIdx <= len(pod.Status.ContainerStatuses)-1 {
						ready = pod.Status.ContainerStatuses[containerIdx].Ready
					}

					if ready {
						nodes = append(nodes, fmt.Sprintf("%s:%d:%d", pod.Status.PodIP, peerPort, port.ContainerPort))
					} else {
						if pod.CreationTimestamp.Add(3 * time.Minute).Before(time.Now().UTC()) {
							err = deleteNode(namespace, pod.Name)
							if err != nil {
								logger.Error(fmt.Sprintf("failed to delete pod: %s", err), "creationTimestamp", pod.CreationTimestamp)
							}
						}
					}
				}
			}
		}
	}

	typesenseNodes := strings.Join(nodes, ",")

	if len(nodes) != 0 {
		logger.Info("new group configuration", "nodes", len(nodes), "group", typesenseNodes)
	}

	return typesenseNodes
}

func deleteNode(namespace, podName string) error {
	return clientset.CoreV1().Pods(namespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
}

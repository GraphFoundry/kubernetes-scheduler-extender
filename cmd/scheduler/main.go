package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	schedulerName = "my-scheduler"
	// host.minikube.internal resolves to the host machine from within Minikube
	extenderURL = "http://host.minikube.internal:9000/prioritize"
)

func main() {
	fmt.Println("Starting custom scheduler...")

	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Printf("Failed to get in-cluster config, trying kubeconfig: %v\n", err)
		kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
		if envVar := os.Getenv("KUBECONFIG"); envVar != "" {
			kubeconfig = envVar
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// Watch loop
	for {
		pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.schedulerName=%s,spec.nodeName=", schedulerName),
		})
		if err != nil {
			fmt.Printf("Error listing pods: %v\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, pod := range pods.Items {
			fmt.Printf("Attempting to schedule pod: %s/%s\n", pod.Namespace, pod.Name)
			err := schedulePod(clientset, &pod)
			if err != nil {
				fmt.Printf("Error scheduling pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
			} else {
				fmt.Printf("Successfully scheduled pod %s/%s\n", pod.Namespace, pod.Name)
			}
		}

		time.Sleep(2 * time.Second)
	}
}
func schedulePod(clientset *kubernetes.Clientset, pod *v1.Pod) error {
	ctx := context.TODO()
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	if len(nodes.Items) == 0 {
		return fmt.Errorf("no nodes available for scheduling pod %s/%s", pod.Namespace, pod.Name)
	}

	nodeNames := make([]string, len(nodes.Items))
	for i, node := range nodes.Items {
		nodeNames[i] = node.Name
	}
	log.Printf("Scheduling pod %s/%s. Available nodes: %v", pod.Namespace, pod.Name, nodeNames)

	if err := callExtender(pod, nodes.Items); err != nil {
		log.Printf("Extender at %s failed for pod %s/%s: %v. Falling back to random selection.", extenderURL, pod.Namespace, pod.Name, err)
	}

	selectedNode := nodes.Items[rand.Intn(len(nodes.Items))]
	log.Printf("Selected node %s for pod %s/%s", selectedNode.Name, pod.Namespace, pod.Name)

	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		Target: v1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       selectedNode.Name,
		},
	}

	return clientset.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
}

func callExtender(pod *v1.Pod, nodes []v1.Node) error {
	// Construct a simple payload. The actual payload depends on the extender's expectation.
	// Based on "Prioritize", it likely expects ExtenderArgs.
	// We'll send a dummy valid JSON to just trigger the endpoint.
	payload := map[string]interface{}{
		"Pod":   pod,
		"Nodes": nodes,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", extenderURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("extender returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

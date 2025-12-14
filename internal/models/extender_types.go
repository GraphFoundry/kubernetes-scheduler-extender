package models

import v1 "k8s.io/api/core/v1"

type ExtenderArgs struct {
	Pod       v1.Pod     `json:"Pod"`
	Nodes     []v1.Node  `json:"Nodes"`
	NodeNames *[]string  `json:"nodenames"`
}

type PodRef struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	UID       string            `json:"uid"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type NodeList struct {
	Items []v1.Node `json:"items"`
}

type Node struct {
	Metadata Metadata `json:"metadata"`
}

type Metadata struct {
	Name string `json:"name"`
}

type ExtenderFilterResult struct {
	Nodes       []v1.Node         `json:"nodes,omitempty"`
	NodeNames   *[]string         `json:"nodenames,omitempty"`
	FailedNodes map[string]string `json:"failedNodes,omitempty"`
	Error       string            `json:"error,omitempty"`
}

type HostPriority struct {
	Host  string `json:"host"`
	Score int    `json:"score"` // 0..100
}

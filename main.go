package main

import (
	"fmt"
	"k8s.io/apimachinery/pkg/runtime"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

var groups = []runtime.SchemeBuilder{
	kubevirtv1.SchemeBuilder,
}
var scheme *runtime.Scheme

func main() {
	fmt.Println("Kubevirt Fuzzer is a tool to fuzz the kubevirt APIs for checking API compatibility " +
		"on upgrades via simple unit tests")
}

package main

import (
	"testing"

	kubevirtfuzzerapitesting "github.com/alaypatel07/kubevirt-fuzzer/apitesting"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestCompatibility(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, builder := range groups {
		require.NoError(t, builder.AddToScheme(scheme))
	}
	kubevirtfuzzerapitesting.NewCompatibilityTestOptions(scheme).Complete(t).Run(t)
}

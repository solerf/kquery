package tree

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func toUIDMap(in map[string]*unstructured.Unstructured) map[types.UID]*unstructured.Unstructured {
	out := make(map[types.UID]*unstructured.Unstructured, len(in))
	for k, v := range in {
		out[types.UID(k)] = v
	}
	return out
}

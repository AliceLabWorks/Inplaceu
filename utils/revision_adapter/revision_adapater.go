package revisionadapter

import (
	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Interface interface {
	EqualToRevisionHash(controllerKey string, obj metav1.Object, hash string) bool
	WriteRevisionHash(obj metav1.Object, hash string)
}

func NewDefaultImpl() Interface {
	return &defaultImpl{}
}

type defaultImpl struct{}

func (r *defaultImpl) EqualToRevisionHash(_ string, obj metav1.Object, hash string) bool {
	return obj.GetLabels()[apps.ControllerRevisionHashLabelKey] == hash
}

func (r *defaultImpl) WriteRevisionHash(obj metav1.Object, hash string) {
	if obj.GetLabels() == nil {
		obj.SetLabels(make(map[string]string, 1))
	}
	obj.GetLabels()[apps.ControllerRevisionHashLabelKey] = hash
}

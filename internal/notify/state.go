package notify

import (
	"context"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Sent records, per ChangeRequest, which "event/channel" pairs were delivered.
type Sent map[string]map[string]bool

// State loads and saves what has been sent.
type State interface {
	// Load returns what was sent. fresh is true when nothing has ever been
	// saved, i.e. on the notifier's first start.
	Load(ctx context.Context) (sent Sent, fresh bool, err error)
	Save(ctx context.Context, sent Sent) error
}

// ConfigMapState keeps Sent in a ConfigMap: one key per ChangeRequest, whose
// value is a comma-separated list of event/channel pairs. Run one notifier:
// a second one would conflict on every save.
type ConfigMapState struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string

	exists          bool
	resourceVersion string
}

func (s *ConfigMapState) Load(ctx context.Context) (Sent, bool, error) {
	cm, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s.exists, s.resourceVersion = false, ""
		return Sent{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	s.exists, s.resourceVersion = true, cm.ResourceVersion
	sent := Sent{}
	for name, v := range cm.Data {
		set := map[string]bool{}
		for _, k := range strings.Split(v, ",") {
			if k != "" {
				set[k] = true
			}
		}
		sent[name] = set
	}
	return sent, false, nil
}

func (s *ConfigMapState) Save(ctx context.Context, sent Sent) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name, Namespace: s.Namespace, ResourceVersion: s.resourceVersion,
			Labels: map[string]string{"app.kubernetes.io/name": "kubeamber-notify", "app.kubernetes.io/part-of": "kubeamber"},
		},
		Data: map[string]string{},
	}
	for name, set := range sent {
		keys := make([]string, 0, len(set))
		for k := range set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		cm.Data[name] = strings.Join(keys, ",")
	}
	var saved *corev1.ConfigMap
	var err error
	if s.exists {
		// The resourceVersion read at Load makes a concurrent writer fail
		// with a conflict instead of being overwritten.
		saved, err = s.Client.CoreV1().ConfigMaps(s.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	} else {
		saved, err = s.Client.CoreV1().ConfigMaps(s.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	}
	if err != nil {
		return err
	}
	s.exists, s.resourceVersion = true, saved.ResourceVersion
	return nil
}

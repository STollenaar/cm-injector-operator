package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/pkg/errors"
	cachev1alpha1 "github.com/stollenaar/cmstate-injector-operator/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"

	v1admission "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create;delete,versions=v1,name=cmstate-operator-webhook.spices.dev,admissionReviewVersions=v1

type PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

type cmStateCreator struct {
	Client  client.Client
	decoder *admission.Decoder
}

func CMStateCreator(mgr ctrl.Manager) error {
	hookServer := mgr.GetWebhookServer()
	hookServer.Register("/mutate-v1-pod", &webhook.Admission{Handler: &cmStateCreator{Client: mgr.GetClient()}})
	return nil
}

// cmStateCreator creates the cmstate if needed or patches the audience.
func (hook *cmStateCreator) Handle(ctx context.Context, req admission.Request) admission.Response {
	resp, err := hook.handleInner(ctx, req)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return *resp
}

func (hook *cmStateCreator) handleInner(ctx context.Context, req admission.Request) (*admission.Response, error) {
	log := ctrl.Log.WithName("webhooks").WithName("CMStateCreator")

	pod := &corev1.Pod{}
	err := hook.decoder.Decode(req, pod)
	if err != nil {
		log.Error(err, "Error decoding request into Pod")
		return nil, errors.Wrap(err, "error decoding request into Pod")
	}

	cmState := &cachev1alpha1.CMState{}
	cmTemplate := &cachev1alpha1.CMTemplate{}
	if pod.Annotations["cache.spices.dev/cmtemplate"] != "" {

		crdName := generateName(pod.Annotations["cache.spices.dev/cmtemplate"])
		err = hook.Client.Get(
			ctx,
			types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      crdName,
			},
			cmState,
		)

		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "fetching cmstate has resulted in an error")
			return nil, errors.Wrap(err, "fetching cmstate has resulted in an error")
		}
		err = hook.Client.Get(
			ctx,
			types.NamespacedName{
				Name: pod.Annotations["cache.spices.dev/cmtemplate"],
			},
			cmTemplate,
		)

		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "fetching cmtemplate has resulted in an error")
			return nil, errors.Wrap(err, "fetching cmtemplate has resulted in an error")
		} else if err != nil {
			log.Error(err, "fetching cmtemplate has resulted in an error")
			return nil, errors.Wrap(err, "fetching cmtemplate has resulted in an error")
		}

		if req.Operation == v1admission.Create {
			return hook.handlePodCreate(req, cmState, cmTemplate, pod, ctx)
		} else if req.Operation == v1admission.Delete {
			return hook.handlePodDelete(cmState, pod, ctx)
		}
	}
	resp := admission.Allowed("skipping cmstate check due to missing annotation")
	return &resp, nil
}

func (hook *cmStateCreator) handlePodDelete(cmState *cachev1alpha1.CMState, pod *corev1.Pod, ctx context.Context) (*admission.Response, error) {
	if cmState.Name == "" {
		resp := admission.Allowed("skipping cmstate patch due to missing cmstate")
		return &resp, nil
	}
	podName := pod.GetName()
	if pod.GetGenerateName() != "" {
		podName = pod.GetGenerateName()
	}

	index := findIndex(cmState.Spec.Audience, podName)
	if index == -1 {
		resp := admission.Allowed("skipping cmstate patch due to pod not in audience")
		return &resp, nil
	}
	cmState.Spec.Audience = append(cmState.Spec.Audience[:index], cmState.Spec.Audience[index+1:]...)

	err := hook.Client.Patch(ctx, cmState, client.Merge)
	if err != nil {
		resp := admission.Denied("patching cmstate has resulted in an error")
		return &resp, err
	}

	resp := admission.Allowed("cmstate has been patched, no need to mutate pod")
	return &resp, nil
}

func (hook *cmStateCreator) handlePodCreate(req admission.Request, cmState *cachev1alpha1.CMState, cmTemplate *cachev1alpha1.CMTemplate, pod *corev1.Pod, ctx context.Context) (*admission.Response, error) {
	if cmState.Name == "" {
		// create the cmstate
		cmState = generateCMState(cmTemplate, pod)

		err := hook.Client.Create(ctx, cmState)

		if err != nil {
			resp := admission.Denied("creating cmstate has resulted in an error")
			return &resp, err
		}
	}

	pod.Annotations["vault.hashicorp.com/agent-configmap"] = cmState.Name

	pData, err := json.Marshal(pod)
	if err != nil {
		return nil, errors.Wrap(err, "error encoding response object")
	}

	resp := admission.PatchResponseFromRaw(req.Object.Raw, pData)
	return &resp, nil
}

// Generating a CMState used for later
func generateCMState(cmTemplate *cachev1alpha1.CMTemplate, pod *corev1.Pod) *cachev1alpha1.CMState {
	annotations := pod.GetAnnotations()

	labels := make(map[string]string)
	for annotation := range cmTemplate.Spec.Template.AnnotationReplace {
		labels[annotation] = annotations[annotation]
	}

	podName := pod.GetName()
	if podName == "" {
		podName = pod.GetGenerateName()
	}
	return &cachev1alpha1.CMState{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cache.spices.dev/v1alpha1",
			Kind:       "CMState",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateName(cmTemplate.Name),
			Namespace: pod.GetNamespace(),
			Labels:    labels,
		},
		Spec: cachev1alpha1.CMStateSpec{
			Audience: []cachev1alpha1.CMAudience{
				{
					Kind: "Pod",
					Name: podName,
				},
			},
			CMTemplate: cmTemplate.Name,
		},
	}
}

func generateName(cmTemplateName string) string {
	return strings.ToLower(strings.ReplaceAll(fmt.Sprintf("cmstate-%s", cmTemplateName), "_", "-"))
}

func findIndex(slice []cachev1alpha1.CMAudience, name string) int {
	for i, aud := range slice {
		if aud.Name == name {
			return i
		}
	}
	return -1
}

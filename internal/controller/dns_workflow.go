package controllers

import (
	"errors"
	"fmt"
	"sync"

	"github.com/samber/lo"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	kuadrantdnsv1alpha1 "github.com/kuadrant/dns-operator/api/v1alpha1"
	"github.com/kuadrant/policy-machinery/controller"
	"github.com/kuadrant/policy-machinery/machinery"

	kuadrantv1 "github.com/kuadrant/kuadrant-operator/api/v1"
	"github.com/kuadrant/kuadrant-operator/internal/kuadrant"
	"github.com/kuadrant/kuadrant-operator/internal/utils"
)

const (
	DNSRecordKind             = "DNSRecord"
	StateDNSPolicyAcceptedKey = "DNSPolicyValid"
	StateDNSPolicyErrorsKey   = "DNSPolicyErrors"

	PolicyConditionSubResourcesHealthy gatewayapiv1alpha2.PolicyConditionType   = "SubResourcesHealthy"
	PolicyReasonSubResourcesHealthy    gatewayapiv1alpha2.PolicyConditionReason = "SubResourcesHealthy"
)

var (
	DNSRecordResource  = kuadrantdnsv1alpha1.GroupVersion.WithResource("dnsrecords")
	DNSRecordGroupKind = schema.GroupKind{Group: kuadrantdnsv1alpha1.GroupVersion.Group, Kind: DNSRecordKind}
)

//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=dnspolicies,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=kuadrant.io,resources=dnspolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kuadrant.io,resources=dnspolicies/finalizers,verbs=update

//+kubebuilder:rbac:groups=kuadrant.io,resources=dnsrecords,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kuadrant.io,resources=dnsrecords/status,verbs=get

func NewDNSWorkflow(client *dynamic.DynamicClient, scheme *runtime.Scheme, isGatewayAPIInstalled, isDNSOperatorInstalled bool) *controller.Workflow {
	return &controller.Workflow{
		Precondition: NewDNSPoliciesValidator(isGatewayAPIInstalled, isDNSOperatorInstalled).Subscription().Reconcile,
		Tasks: []controller.ReconcileFunc{
			NewEffectiveDNSPoliciesReconciler(client, scheme).Subscription().Reconcile,
		},
		Postcondition: NewDNSPolicyStatusUpdater(client).Subscription().Reconcile,
	}
}

func LinkListenerToDNSRecord(objs controller.Store) machinery.LinkFunc {
	gateways := lo.Map(objs.FilterByGroupKind(machinery.GatewayGroupKind), controller.ObjectAs[*gwapiv1.Gateway])
	listeners := lo.FlatMap(lo.Map(gateways, func(g *gwapiv1.Gateway, _ int) *machinery.Gateway {
		return &machinery.Gateway{Gateway: g}
	}), machinery.ListenersFromGatewayFunc)

	return machinery.LinkFunc{
		From: machinery.ListenerGroupKind,
		To:   DNSRecordGroupKind,
		Func: func(child machinery.Object) []machinery.Object {
			return lo.FilterMap(listeners, func(l *machinery.Listener, _ int) (machinery.Object, bool) {
				if dnsRecord, ok := child.(*controller.RuntimeObject).Object.(*kuadrantdnsv1alpha1.DNSRecord); ok {
					return l, l.GetNamespace() == dnsRecord.GetNamespace() &&
						dnsRecord.GetName() == dnsRecordName(l.Gateway.Name, string(l.Name))
				}
				return nil, false
			})
		},
	}
}

func LinkDNSPolicyToDNSRecord(objs controller.Store) machinery.LinkFunc {
	policies := lo.Map(objs.FilterByGroupKind(kuadrantv1.DNSPolicyGroupKind), controller.ObjectAs[*kuadrantv1.DNSPolicy])

	return machinery.LinkFunc{
		From: kuadrantv1.DNSPolicyGroupKind,
		To:   DNSRecordGroupKind,
		Func: func(child machinery.Object) []machinery.Object {
			if dnsRecord, ok := child.(*controller.RuntimeObject).Object.(*kuadrantdnsv1alpha1.DNSRecord); ok {
				return lo.FilterMap(policies, func(dnsPolicy *kuadrantv1.DNSPolicy, _ int) (machinery.Object, bool) {
					return dnsPolicy, utils.IsOwnedBy(dnsRecord, dnsPolicy)
				})
			}
			return nil
		},
	}
}

func dnsPolicyAcceptedStatusFunc(state *sync.Map) func(policy machinery.Policy) (bool, error) {
	validatedPolicies, validated := state.Load(StateDNSPolicyAcceptedKey)
	if !validated {
		return dnsPolicyAcceptedStatus
	}
	validatedPoliciesMap := validatedPolicies.(map[string]error)
	return func(policy machinery.Policy) (bool, error) {
		err, pValidated := validatedPoliciesMap[policy.GetLocator()]
		if pValidated {
			return err == nil, err
		}
		return dnsPolicyAcceptedStatus(policy)
	}
}

func dnsPolicyAcceptedStatus(policy machinery.Policy) (accepted bool, err error) {
	p, ok := policy.(*kuadrantv1.DNSPolicy)
	if !ok {
		return
	}
	if condition := meta.FindStatusCondition(p.Status.Conditions, string(gatewayapiv1alpha2.PolicyConditionAccepted)); condition != nil {
		accepted = condition.Status == metav1.ConditionTrue
		if !accepted {
			err = errors.New(condition.Message)
		}
		return
	}
	return
}

func dnsPolicyErrorFunc(state *sync.Map) func(policy machinery.Policy) error {
	var policyErrorsMap map[string]error
	policyErrors, exists := state.Load(StateDNSPolicyErrorsKey)
	if exists {
		policyErrorsMap = policyErrors.(map[string]error)
	}
	return func(policy machinery.Policy) error {
		return policyErrorsMap[policy.GetLocator()]
	}
}

type dnsPolicyTypeFilter func(item machinery.Policy, index int) (*kuadrantv1.DNSPolicy, bool)

func dnsPolicyTypeFilterFunc() func(item machinery.Policy, _ int) (*kuadrantv1.DNSPolicy, bool) {
	return func(item machinery.Policy, _ int) (*kuadrantv1.DNSPolicy, bool) {
		p, ok := item.(*kuadrantv1.DNSPolicy)
		return p, ok
	}
}

func dnsPolicyHealthyCondition(policy kuadrant.Policy, err kuadrant.PolicyError) *metav1.Condition {
	cond := &metav1.Condition{
		Type:    string(PolicyConditionSubResourcesHealthy),
		Status:  metav1.ConditionTrue,
		Reason:  string(PolicyReasonSubResourcesHealthy),
		Message: fmt.Sprintf("all subresources of %s are healthy", policy.Kind()),
	}

	if err == nil {
		return cond
	}

	// Wrap error into a PolicyError if it is not this type
	var policyErr kuadrant.PolicyError
	if !errors.As(err, &policyErr) {
		policyErr = kuadrant.NewErrUnknown(policy.Kind(), err)
	}
	cond.Status = metav1.ConditionFalse
	cond.Message = policyErr.Error()
	cond.Reason = string(policyErr.Reason())

	return cond
}

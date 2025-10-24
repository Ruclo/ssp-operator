package metrics

import (
	"fmt"

	promv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	v1 "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	template_validator "kubevirt.io/ssp-operator/internal/operands/template-validator"
)

const (
	MonitorNamespace                    = "openshift-monitoring"
	defaultRunbookURLTemplate           = "https://kubevirt.io/monitoring/runbooks/%s"
	runbookURLTemplateEnv               = "RUNBOOK_URL_TEMPLATE"
	PrometheusLabelKey                  = "prometheus.ssp.kubevirt.io"
	PrometheusLabelValue                = "true"
	PrometheusClusterRoleName           = "prometheus-k8s-ssp"
	PrometheusServiceAccountName        = "prometheus-k8s"
	SspMetricsPortName                  = "http-metrics"
	TemplateValidatorMetricsServiceName = "template-validator-metrics"
	SspMetricsServiceName               = "ssp-operator-metrics"
	MetricsServiceKey                   = "metrics.ssp.kubevirt.io"
)

func newMonitoringClusterRole() *rbac.ClusterRole {
	return &rbac.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: PrometheusClusterRoleName,
		},
		Rules: []rbac.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"services", "endpoints", "pods"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
}

func newMonitoringClusterRoleBinding() *rbac.ClusterRoleBinding {
	return &rbac.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: PrometheusClusterRoleName,
		},
		Subjects: []rbac.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      PrometheusServiceAccountName,
				Namespace: MonitorNamespace,
			},
		},
		RoleRef: rbac.RoleRef{
			Kind:     "ClusterRole",
			Name:     PrometheusClusterRoleName,
			APIGroup: rbac.GroupName,
		},
	}
}

func ServiceMonitorLabels() map[string]string {
	return map[string]string{
		"openshift.io/cluster-monitoring": "true",
		PrometheusLabelKey:                PrometheusLabelValue,
		"k8s-app":                         "kubevirt",
	}
}

func serviceCABundle() promv1.SecretOrConfigMap {
	ServiceCABundle := "openshift-service-ca.crt"
	ServiceCABUndleKey := "service-ca.crt"
	return promv1.SecretOrConfigMap{
		ConfigMap: &v1.ConfigMapKeySelector{
			LocalObjectReference: v1.LocalObjectReference{
				Name: ServiceCABundle,
			},
			Key: ServiceCABUndleKey,
		},
	}
}

func getCAConfigForServiceMonitor(olmDeployment bool) promv1.SecretOrConfigMap {
	OLMManagedCert := "ssp-operator-service-cert"
	OLMManagedCertKey := "olmCAKey"
	if olmDeployment {
		//OLM includes the CABundle alongside the certificate itself in the same secret.
		return promv1.SecretOrConfigMap{
			Secret: &v1.SecretKeySelector{
				LocalObjectReference: v1.LocalObjectReference{
					Name: OLMManagedCert,
				},
				Key: OLMManagedCertKey,
			},
		}
	}
	return serviceCABundle()
}

func newValidatorServiceMonitor(namespace string) *promv1.ServiceMonitor {
	tlsConfig := &promv1.TLSConfig{
		SafeTLSConfig: promv1.SafeTLSConfig{
			CA:         serviceCABundle(),
			ServerName: ptr.To(fmt.Sprintf("virt-template-validator.%s.svc", namespace)),
		},
	}
	serviceMonitor := newServiceMonitor(TemplateValidatorMetricsServiceName, template_validator.MetricsPortName, namespace, tlsConfig, metav1.LabelSelector{
		MatchLabels: map[string]string{
			MetricsServiceKey: TemplateValidatorMetricsServiceName,
		},
	})
	return &serviceMonitor
}

func newSspServiceMonitor(namespace string, olmDeployment bool, sspServiceHostname string) *promv1.ServiceMonitor {
	tlsConfig := &promv1.TLSConfig{
		SafeTLSConfig: promv1.SafeTLSConfig{
			CA:         getCAConfigForServiceMonitor(olmDeployment),
			ServerName: ptr.To(sspServiceHostname),
		},
	}
	serviceMonitor := newServiceMonitor(SspMetricsServiceName, SspMetricsPortName, namespace, tlsConfig, metav1.LabelSelector{
		MatchLabels: map[string]string{
			MetricsServiceKey: SspMetricsServiceName,
		},
	})
	return &serviceMonitor
}

func ValidatorMetricsServiceLabels() map[string]string {
	return map[string]string{
		PrometheusLabelKey: PrometheusLabelValue,
		MetricsServiceKey:  TemplateValidatorMetricsServiceName,
	}
}

func newValidatorMetricsService(namespace string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      TemplateValidatorMetricsServiceName,
			Namespace: namespace,
			Labels:    ValidatorMetricsServiceLabels(),
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       template_validator.MetricsPortName,
					Port:       443,
					Protocol:   v1.ProtocolTCP,
					TargetPort: intstr.FromString(template_validator.MetricsPortName),
				},
			},
			Selector: map[string]string{
				"name": template_validator.DeploymentName,
			},
		},
	}
}

func SspMetricsServiceLabels() map[string]string {
	return map[string]string{
		PrometheusLabelKey: PrometheusLabelValue,
		MetricsServiceKey:  SspMetricsServiceName,
	}
}

func newSspMetricsService(namespace string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SspMetricsServiceName,
			Namespace: namespace,
			Labels:    SspMetricsServiceLabels(),
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       SspMetricsPortName,
					Port:       443,
					Protocol:   v1.ProtocolTCP,
					TargetPort: intstr.FromString(SspMetricsPortName),
				},
			},
			Selector: map[string]string{
				"name": "ssp-operator",
			},
		},
	}
}

func newServiceMonitor(name,
	port,
	namespace string,
	tlsConfig *promv1.TLSConfig,
	selector metav1.LabelSelector) promv1.ServiceMonitor {
	return promv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    ServiceMonitorLabels(),
		},
		Spec: promv1.ServiceMonitorSpec{
			NamespaceSelector: promv1.NamespaceSelector{
				Any: true,
			},
			Selector: selector,
			Endpoints: []promv1.Endpoint{
				{
					Port:        port,
					Scheme:      "https",
					TLSConfig:   tlsConfig,
					HonorLabels: true,
				},
			},
		},
	}
}

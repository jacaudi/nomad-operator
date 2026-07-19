package controller

import (
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

const (
	portHTTP = 4646
	portRPC  = 4647
	portSerf = 4648
)

func buildConfigMap(nc *nomadv1alpha1.NomadCluster, gatewayAddress string) *corev1.ConfigMap {
	n := names(nc)
	body, _ := renderConfig(nc, gatewayAddress)
	var sb strings.Builder
	for _, p := range rpcAdvertisePorts(nc) {
		sb.WriteString(" ")
		sb.WriteString(itoa(int(p)))
	}
	ports := trimLeadingSpace(sb.String())
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: n.ConfigMap, Namespace: nc.Namespace, Labels: n.Labels()},
		Data: map[string]string{
			"server.hcl":      body,
			"gateway_address": gatewayAddress,
			"rpc_ports":       ports,
			"rpc_advertise":   rpcAdvertiseStrategy(nc),
			"entrypoint.sh":   initEntrypoint,
		},
	}
}

// rpcAdvertise* are the two raft RPC advertise strategies the init entrypoint
// branches on (read from ConfigMap key "rpc_advertise"). The literal
// rpcAdvertisePod value MUST stay in sync with the `= "pod"` test in
// initEntrypoint below.
const (
	rpcAdvertisePod      = "pod"
	rpcAdvertiseExternal = "external"
)

// rpcAdvertiseStrategy selects how each server advertises its raft RPC address.
// A single-voter raft (servers==1 — the LoadBalancer case and the single-node
// Gateway case alike) must advertise a STABLE external address (${GW}:${RPCPORT}):
// its pod IP drifts on restart and a lone voter cannot be removed from its own
// peer set, so a drifting self-address wedges raft (slice-6b). A multi-voter raft
// (servers 3/5) advertises its pod-network address (${POD_IP}:4647): peers reach a
// remote server at (that server's serf IP = POD_IP) + (its advertised RPC port),
// so the advertised port MUST be the 4647 the server actually binds — advertising
// the per-ordinal EXTERNAL port leaves peers dialing a port nothing listens on
// (connection refused → no leader). Autopilot self-heals POD_IP churn. The
// predicate keys on servers, not mode, because single-voter wedge risk is a
// property of the voter count.
func rpcAdvertiseStrategy(nc *nomadv1alpha1.NomadCluster) string {
	if nc.Spec.Servers == 1 {
		return rpcAdvertiseExternal
	}
	return rpcAdvertisePod
}

// initEntrypoint runs in the init container: it copies the shared server.hcl and
// writes a SECOND overlay file (loaded because the agent uses -config=<dir>) that
// carries the per-pod advertise stanza AND the gossip encrypt key read from the
// mounted gossip Secret. Nomad deep-merges the two server{} blocks across files,
// so bootstrap_expect/server_join (base) + encrypt (overlay) combine.
//
// advertise.rpc is mode-aware (see rpcAdvertiseStrategy): "pod" advertises the
// pod-network ${POD_IP}:4647 that a multi-voter server actually binds (so raft
// peers dialing serfIP:advertisedPort reach a live listener), while any other
// value keeps the external-stable ${GW}:${RPCPORT} a single voter needs. The
// `= "pod"` literal below MUST match the rpcAdvertisePod constant.
const initEntrypoint = `#!/bin/sh
set -eu
ORD="${HOSTNAME##*-}"
PORTS="$(cat /config/rpc_ports)"
GW="$(cat /config/gateway_address)"
KEY="$(cat /nomad/gossip/key)"
i=0; RPCPORT=""
for p in $PORTS; do if [ "$i" = "$ORD" ]; then RPCPORT="$p"; fi; i=$((i+1)); done
if [ "$(cat /config/rpc_advertise)" = "pod" ]; then RPCADV="${POD_IP}:4647"; else RPCADV="${GW}:${RPCPORT}"; fi
cp /config/server.hcl /nomad/config/server.hcl
cat > /nomad/config/overlay.hcl <<EOF
server {
  encrypt = "${KEY}"
}
advertise {
  http = "${GW}:4646"
  rpc  = "${RPCADV}"
  serf = "${POD_IP}"
}
EOF
`

func buildHeadlessService(nc *nomadv1alpha1.NomadCluster) *corev1.Service {
	n := names(nc)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: n.HeadlessSvc, Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 n.Labels(),
			Ports: []corev1.ServicePort{
				{Name: "serf-tcp", Port: portSerf, Protocol: corev1.ProtocolTCP},
				{Name: "serf-udp", Port: portSerf, Protocol: corev1.ProtocolUDP},
				{Name: "rpc", Port: portRPC, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func buildPodService(nc *nomadv1alpha1.NomadCluster, ordinal int) *corev1.Service {
	n := names(nc)
	sel := n.Labels()
	sel["statefulset.kubernetes.io/pod-name"] = n.PodName(ordinal)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: n.PodSvc(ordinal), Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			PublishNotReadyAddresses: true,
			Selector:                 sel,
			Ports:                    []corev1.ServicePort{{Name: "rpc", Port: portRPC, TargetPort: intstr.FromInt32(portRPC), Protocol: corev1.ProtocolTCP}},
		},
	}
}

func buildAPIService(nc *nomadv1alpha1.NomadCluster) *corev1.Service {
	n := names(nc)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: n.APISvc, Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: n.Labels(),
			Ports:    []corev1.ServicePort{{Name: "http", Port: portHTTP, TargetPort: intstr.FromInt32(portHTTP), Protocol: corev1.ProtocolTCP}},
		},
	}
}

func buildPDB(nc *nomadv1alpha1.NomadCluster) *policyv1.PodDisruptionBudget {
	n := names(nc)
	minAvail := intstr.FromInt(int(nc.Spec.Servers) - 1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: n.PDB, Namespace: nc.Namespace, Labels: n.Labels()},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: n.Labels()},
		},
	}
}

func buildStatefulSet(nc *nomadv1alpha1.NomadCluster, configHash string) *appsv1.StatefulSet {
	n := names(nc)
	replicas := nc.Spec.Servers
	labels := n.Labels()
	storageQty := resource.MustParse(nc.Spec.Storage.Size)

	// Readiness is leader-gated but must be an EXEC probe: verify_https_client=true
	// requires a client cert on every HTTPS request, which an httpGet probe cannot
	// present. `nomad operator api` reads NOMAD_CACERT/NOMAD_CLIENT_CERT/
	// NOMAD_CLIENT_KEY (set on the container) and exits non-zero on HTTP 500
	// ("no leader"). The 127.0.0.1 SAN required by the design makes the localhost
	// dial verify.
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"nomad", "operator", "api", "/v1/agent/health?type=server"},
		}},
		InitialDelaySeconds: 10, PeriodSeconds: 10, FailureThreshold: 6,
	}
	liveness := &corev1.Probe{ // process-level, NOT leader-gated
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(portRPC)}},
		InitialDelaySeconds: 30, PeriodSeconds: 30, FailureThreshold: 5,
	}

	tmplAnnotations := map[string]string{"nomad.operator.io/config-hash": configHash}

	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storageQty}},
		},
	}
	if nc.Spec.Storage.StorageClassName != "" {
		pvc.Spec.StorageClassName = &nc.Spec.Storage.StorageClassName
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: n.StatefulSet, Namespace: nc.Namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:          n.HeadlessSvc,
			Replicas:             &replicas,
			PodManagementPolicy:  appsv1.ParallelPodManagement,
			UpdateStrategy:       appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			Selector:             &metav1.LabelSelector{MatchLabels: labels},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{pvc},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: tmplAnnotations},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							TopologyKey:   "kubernetes.io/hostname",
							LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
						}},
					}},
					InitContainers: []corev1.Container{{
						Name:    "render-config",
						Image:   nc.Spec.Image,
						Command: []string{"/bin/sh", "/config/entrypoint.sh"},
						Env:     []corev1.EnvVar{{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/config"},
							{Name: "rendered", MountPath: "/nomad/config"},
							{Name: "gossip", MountPath: "/nomad/gossip", ReadOnly: true},
						},
					}},
					Containers: []corev1.Container{{
						Name:    "nomad",
						Image:   nc.Spec.Image,
						Command: []string{"nomad", "agent", "-config=/nomad/config"}, // directory: server.hcl + overlay.hcl merge
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: portHTTP}, {Name: "rpc", ContainerPort: portRPC},
							{Name: "serf-tcp", ContainerPort: portSerf, Protocol: corev1.ProtocolTCP},
							{Name: "serf-udp", ContainerPort: portSerf, Protocol: corev1.ProtocolUDP},
						},
						Env: []corev1.EnvVar{
							{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
							{Name: "NOMAD_ADDR", Value: "https://127.0.0.1:4646"},
							{Name: "NOMAD_CACERT", Value: "/nomad/tls/ca.crt"},
							{Name: "NOMAD_CLIENT_CERT", Value: "/nomad/tls/tls.crt"},
							{Name: "NOMAD_CLIENT_KEY", Value: "/nomad/tls/tls.key"},
						},
						ReadinessProbe: probe,
						LivenessProbe:  liveness,
						Resources:      nc.Spec.Resources,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "rendered", MountPath: "/nomad/config"},
							{Name: "data", MountPath: "/var/lib/nomad"},
							{Name: "tls", MountPath: "/nomad/tls", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: n.ConfigMap}}}},
						{Name: "rendered", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: nc.Spec.TLS.CertSecretRef}}},
						{Name: "gossip", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: n.GossipSecret}}},
					},
				},
			},
		},
	}
}

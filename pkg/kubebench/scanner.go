package kubebench

import (
	"context"
	"fmt"

	"github.com/aquasecurity/starboard/pkg/scanners"

	"k8s.io/klog"

	starboard "github.com/aquasecurity/starboard/pkg/apis/aquasecurity/v1alpha1"

	"github.com/aquasecurity/starboard/pkg/kube"
	"github.com/aquasecurity/starboard/pkg/kube/pod"
	"github.com/aquasecurity/starboard/pkg/runner"
	"github.com/google/uuid"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	"k8s.io/client-go/kubernetes"
)

const (
	kubeBenchContainerName  = "kube-bench"
	kubeBenchContainerImage = "aquasec/kube-bench:latest"
)

type Scanner struct {
	opts      kube.ScannerOpts
	clientset kubernetes.Interface
	pods      *pod.Manager
	converter Converter
	scanners.Base
}

func NewScanner(opts kube.ScannerOpts, clientset kubernetes.Interface) *Scanner {
	return &Scanner{
		opts:      opts,
		clientset: clientset,
		pods:      pod.NewPodManager(clientset),
		converter: DefaultConverter,
	}
}

func (s *Scanner) Scan(ctx context.Context) (report starboard.CISKubeBenchOutput, node *core.Node, err error) {
	// 1. Prepare descriptor for the Kubernetes Job which will run kube-bench
	kubeBenchJob := s.prepareKubeBenchJob()

	// 2. Run the prepared Job and wait for its completion or failure
	err = runner.New().Run(ctx, kube.NewRunnableJob(s.clientset, kubeBenchJob))
	if err != nil {
		err = fmt.Errorf("running kube-bench job: %w", err)
		return
	}

	defer func() {
		// 6. Delete the kube-bench Job
		klog.V(3).Infof("Deleting job: %s/%s", kubeBenchJob.Namespace, kubeBenchJob.Name)
		background := meta.DeletePropagationBackground
		_ = s.clientset.BatchV1().Jobs(kubeBenchJob.Namespace).Delete(ctx, kubeBenchJob.Name, meta.DeleteOptions{
			PropagationPolicy: &background,
		})
	}()

	// 3. Get the Pod controlled by the kube-bench Job
	kubeBenchPod, err := s.pods.GetPodByJob(ctx, kubeBenchJob)
	if err != nil {
		err = fmt.Errorf("getting kube-bench pod: %w", err)
		return
	}

	// 4. Get kube-bench JSON output from the kube-bench Pod
	klog.V(3).Infof("Getting logs for %s container in job: %s/%s", kubeBenchContainerName,
		kubeBenchJob.Namespace, kubeBenchJob.Name)
	logsReader, err := s.pods.GetPodLogs(ctx, kubeBenchPod, kubeBenchContainerName)
	if err != nil {
		err = fmt.Errorf("getting logs: %w", err)
		return
	}
	defer func() {
		_ = logsReader.Close()
	}()

	// 5. Parse the CISBenchmarkReport from the logs Reader
	report, err = s.converter.Convert(logsReader)
	if err != nil {
		err = fmt.Errorf("parsing CIS benchmark report: %w", err)
		return
	}

	node, err = s.clientset.CoreV1().Nodes().Get(ctx, kubeBenchPod.Spec.NodeName, meta.GetOptions{})
	return
}

func (s *Scanner) prepareKubeBenchJob() *batch.Job {
	return &batch.Job{
		ObjectMeta: meta.ObjectMeta{
			Name:      uuid.New().String(),
			Namespace: kube.NamespaceStarboard,
			Labels: map[string]string{
				"app": "kube-bench",
			},
		},
		Spec: batch.JobSpec{
			BackoffLimit:          pointer.Int32Ptr(0),
			Completions:           pointer.Int32Ptr(1),
			ActiveDeadlineSeconds: s.GetActiveDeadlineSeconds(s.opts.ScanJobTimeout),
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{
						"app": "kube-bench",
					},
				},
				Spec: core.PodSpec{
					RestartPolicy: core.RestartPolicyNever,
					HostPID:       true,
					Volumes: []core.Volume{
						{
							Name: "var-lib-etcd",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: "/var/lib/etcd",
								},
							},
						},
						{
							Name: "var-lib-kubelet",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: "/var/lib/kubelet",
								},
							},
						},
						{
							Name: "etc-systemd",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: "/etc/systemd",
								},
							},
						},
						{
							Name: "etc-kubernetes",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: "/etc/kubernetes",
								},
							},
						},
						{
							Name: "usr-bin",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: "/usr/bin",
								},
							},
						},
					},
					Containers: []core.Container{
						{
							Name:                     kubeBenchContainerName,
							Image:                    kubeBenchContainerImage,
							ImagePullPolicy:          core.PullAlways,
							TerminationMessagePolicy: core.TerminationMessageFallbackToLogsOnError,
							Command:                  []string{"kube-bench"},
							Args:                     []string{"--json"},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "var-lib-etcd",
									MountPath: "/var/lib/etcd",
									ReadOnly:  true,
								},
								{
									Name:      "var-lib-kubelet",
									MountPath: "/var/lib/kubelet",
									ReadOnly:  true,
								},
								{
									Name:      "etc-systemd",
									MountPath: "/etc/systemd",
									ReadOnly:  true,
								},
								{
									Name:      "etc-kubernetes",
									MountPath: "/etc/kubernetes",
									ReadOnly:  true,
								},
								{
									Name:      "usr-bin",
									MountPath: "/usr/local/mount-from-host/bin",
									ReadOnly:  true,
								},
							},
						},
					},
				},
			},
		},
	}
}

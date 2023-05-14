package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh/terminal"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	execName = "hackctl"
)

func main() {
	// TODO around here, reset the whole logger thing to a nice format

	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %+v\nstack:\n%v", r, string(debug.Stack()))
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	flagSet := flag.NewFlagSet(execName, flag.ExitOnError)

	flagSet.SetOutput(os.Stdout)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Usage: %s [OPTIONS]\n"+
			"Hacks a shell into a Kubernetes node. Example:\n\n"+
			"\t%s --context dummy-ctx --node dummy-node\n\n"+
			"Options:\n", os.Args[0], os.Args[0])
		flagSet.PrintDefaults()
	}

	var (
		kubeContext string
		nodeName    string
		namespace   string
		image       string
		kubeConfig  string
		masterURL   string
		help        bool
	)

	namespace = "default"

	flagSet.StringVar(&kubeContext, "context", "", "A context name of the given, or default, kube config.")
	flagSet.StringVar(&nodeName, "node", "", "Node name in which the shell should start. It is assumed that the nodes are labeled with the key kubernetes.io/hostname.")
	flagSet.StringVar(&image, "image", "", "Full image path for the shell that will be opened. Defaults to the one constructed from the Dockerfile present in github.com/luisferreira32/hackctl.")
	flagSet.StringVar(&kubeConfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster. Defaults to $HOME/.kube/config.")
	flagSet.StringVar(&masterURL, "masterurl", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster. Defaults to values available in a kube config.")
	flagSet.BoolVar(&help, "help", false, "To print this help again.")

	flagSet.Parse(os.Args[1:])

	if help {
		flagSet.Usage()
		return
	}

	if image == "" {
		image = "ubuntu" // TODO: fix me to the one constructed from the docker file
	}

	if kubeConfig == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Panicf("could not find user home dir %v", err)
		}
		kubeConfig = homeDir + "/.kube/config"
	}

	configLoadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeConfig}
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: kubeContext,
		ClusterInfo:    clientcmdapi.Cluster{Server: masterURL},
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(configLoadingRules, configOverrides).ClientConfig()
	if err != nil {
		log.Panicf("creating kube rest config %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Panicf("building kubernetes clientset: %v", err)
	}

	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("failed to retrieve nodes list: %v", err)
	}

	if nodes != nil {
		if nodeName == "" && len(nodes.Items) > 0 {
			nodeName = nodes.Items[0].Name
		}
		for i := 0; i < len(nodes.Items); i++ {
			node := nodes.Items[i]
			if node.Name == nodeName {
				break // found what we're looking for
			}
		}
	}
	if nodeName == "" {
		log.Panic("cannot attach to a node if none is given or found")
	}

	podName := execName + uuid.NewString()

	valTrue := true
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": nodeName,
			},
			Containers: []corev1.Container{
				{
					Name:  podName,
					Image: image,
					VolumeMounts: []corev1.VolumeMount{
						{
							MountPath: "/host",
							Name:      "host-root",
						},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &valTrue,
					},
					Command: []string{"/bin/bash", "-c", "tail -f /dev/null"},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "host-root",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/"},
					},
				},
			},
			Tolerations: []corev1.Toleration{
				{
					Operator: corev1.TolerationOpExists, // we want to be schedulable no matter what
				},
			},
		},
	}

	_, err = kubeClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		log.Panicf("%v", err)
	}

	watcher, err := kubeClient.CoreV1().Pods(namespace).Watch(ctx, metav1.SingleObject(pod.ObjectMeta))
	if err != nil {
		log.Panic(err)
	}

	// TODO actually wait container ready and not just pod?
	func() {
		for {
			select {
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return
				}
				switch event.Type {
				case watch.Modified:
					pod = event.Object.(*corev1.Pod)
					for _, cond := range pod.Status.Conditions {
						if cond.Type == corev1.PodReady &&
							cond.Status == corev1.ConditionTrue {
							watcher.Stop()
						}
					}
				default:
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	defer func() {
		err = kubeClient.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		if err != nil {
			log.Panicf("%v", err)
		}
	}()

	req := kubeClient.RESTClient().
		Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: podName,
			Command:   []string{"/bin/bash"},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	// TODO unable to upgrade connection: 404 page not found ... fix this
	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		log.Panicf("%v", err)
	}

	oldState, err := terminal.MakeRaw(0)
	if err != nil {
		log.Panicf("%v", err)
	}
	defer terminal.Restore(0, oldState)

	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Tty:    true,
	})
	if err != nil {
		log.Printf("oops %v", err)
	}
}

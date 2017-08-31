/*
Copyright 2016 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package rgw for the Ceph object store.
package rgw

import (
	"encoding/json"
	"fmt"

	"github.com/coreos/pkg/capnslog"
	"github.com/rook/rook/pkg/ceph/client"
	cephrgw "github.com/rook/rook/pkg/ceph/rgw"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/operator/k8sutil"
	opmon "github.com/rook/rook/pkg/operator/mon"
	"k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", "op-rgw")

const (
	appName     = "rook-ceph-rgw"
	keyringName = "keyring"
)

// Cluster for rgw management
type Cluster struct {
	context   *clusterd.Context
	Name      string
	Namespace string
	placement k8sutil.Placement
	Version   string
	Replicas  int32
}

// New creates an instance of an rgw manager
func New(context *clusterd.Context, name, namespace, version string, placement k8sutil.Placement) *Cluster {
	return &Cluster{
		context:   context,
		Name:      name,
		Namespace: namespace,
		placement: placement,
		Version:   version,
		Replicas:  2,
	}
}

// Start the rgw manager
func (c *Cluster) Start() error {
	logger.Infof("start running rgw")

	err := c.createKeyring()
	if err != nil {
		return fmt.Errorf("failed to create rgw keyring. %+v", err)
	}

	// start the service
	serviceIP, err := c.startService()
	if err != nil {
		return fmt.Errorf("failed to start rgw service. %+v", err)
	}

	err = c.createRealm(serviceIP)

	// start the deployment
	deployment := c.makeDeployment()
	_, err = c.context.Clientset.ExtensionsV1beta1().Deployments(c.Namespace).Create(deployment)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create rgw deployment. %+v", err)
		}
		logger.Infof("rgw deployment already exists")
	} else {
		logger.Infof("rgw deployment started")
	}

	return nil
}

type idType struct {
	ID string `json:"id"`
}

func (c *Cluster) createRealm(serviceIP string) error {
	output, err := c.runRGWCommand("realm", "create", fmt.Sprintf("--rgw-realm=%s", c.Name))
	if err != nil {
		return fmt.Errorf("failed to create rgw realm %s. %+v", c.Name, err)
	}

	realmID, err := decodeID(output)
	if err != nil {
		return fmt.Errorf("failed to parse realm id. %+v", err)
	}

	output, err = c.runRGWCommand("zonegroup", "create", "--master",
		fmt.Sprintf("--endpoints=%s:%d", serviceIP, cephrgw.RGWPort),
		fmt.Sprintf("--rgw-zonegroup=%s", c.Name),
		fmt.Sprintf("--rgw-realm=%s", c.Name))
	if err != nil {
		return fmt.Errorf("failed to create rgw zonegroup for %s. %+v", c.Name, err)
	}

	zoneGroupID, err := decodeID(output)
	if err != nil {
		return fmt.Errorf("failed to parse realm id. %+v", err)
	}

	output, err = c.runRGWCommand("zone", "create", "--master",
		fmt.Sprintf("--endpoints=%s:%d", serviceIP, cephrgw.RGWPort),
		fmt.Sprintf("--rgw-zone=%s", c.Name),
		fmt.Sprintf("--rgw-zonegroup=%s", c.Name),
		fmt.Sprintf("--rgw-realm=%s", c.Name))
	if err != nil {
		return fmt.Errorf("failed to create rgw zonegroup for %s. %+v", c.Name, err)
	}

	zoneID, err := decodeID(output)
	if err != nil {
		return fmt.Errorf("failed to parse zone id. %+v", err)
	}

	logger.Infof("RGW: realm=%s, zonegroup=%s, zone=%s", realmID, zoneGroupID, zoneID)
	return nil
}

func decodeID(data string) (string, error) {
	var id idType
	err := json.Unmarshal([]byte(data), &id)
	if err != nil {
		return "", fmt.Errorf("Failed to unmarshal json: %+v", err)
	}

	return id.ID, err
}

func (c *Cluster) runRGWCommand(args ...string) (string, error) {
	options := client.AppendAdminConnectionArgs(args, c.context.ConfigDir, c.Namespace)

	// start the rgw admin command
	output, err := c.context.Executor.ExecuteCommandWithCombinedOutput(false, "", "radosgw-admin", options...)
	if err != nil {
		return "", fmt.Errorf("failed to run radosgw-admin: %+v", err)
	}
	return output, nil
}

func (c *Cluster) createKeyring() error {
	_, err := c.context.Clientset.CoreV1().Secrets(c.Namespace).Get(c.instanceName(), metav1.GetOptions{})
	if err == nil {
		logger.Infof("the rgw keyring was already generated")
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get rgw secrets. %+v", err)
	}

	// create the keyring
	logger.Infof("generating rgw keyring")
	keyring, err := cephrgw.CreateKeyring(c.context, c.Namespace)
	if err != nil {
		return fmt.Errorf("failed to create keyring. %+v", err)
	}

	// store the secrets
	secrets := map[string]string{
		keyringName: keyring,
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: c.instanceName(), Namespace: c.Namespace},
		StringData: secrets,
		Type:       k8sutil.RookType,
	}
	_, err = c.context.Clientset.CoreV1().Secrets(c.Namespace).Create(secret)
	if err != nil {
		return fmt.Errorf("failed to save rgw secrets. %+v", err)
	}

	return nil
}

func (c *Cluster) instanceName() string {
	return InstanceName(c.Name)
}

func InstanceName(name string) string {
	return fmt.Sprintf("%s-%s", appName, name)
}

func (c *Cluster) makeDeployment() *extensions.Deployment {
	deployment := &extensions.Deployment{}
	deployment.Name = c.instanceName()
	deployment.Namespace = c.Namespace

	podSpec := v1.PodSpec{
		Containers:    []v1.Container{c.rgwContainer()},
		RestartPolicy: v1.RestartPolicyAlways,
		Volumes: []v1.Volume{
			{Name: k8sutil.DataDirVolume, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
			k8sutil.ConfigOverrideVolume(),
		},
	}
	c.placement.ApplyToPodSpec(&podSpec)

	podTemplateSpec := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "rook-ceph-rgw",
			Labels:      c.getLabels(),
			Annotations: map[string]string{},
		},
		Spec: podSpec,
	}

	deployment.Spec = extensions.DeploymentSpec{Template: podTemplateSpec, Replicas: &c.Replicas}

	return deployment
}

func (c *Cluster) rgwContainer() v1.Container {

	return v1.Container{
		Args: []string{
			"rgw",
			fmt.Sprintf("--config-dir=%s", k8sutil.DataDir),
			fmt.Sprintf("--rgw-name=%s", c.Name),
			fmt.Sprintf("--rgw-port=%d", cephrgw.RGWPort),
			fmt.Sprintf("--rgw-host=%s", cephrgw.DNSName),
		},
		Name:  c.instanceName(),
		Image: k8sutil.MakeRookImage(c.Version),
		VolumeMounts: []v1.VolumeMount{
			{Name: k8sutil.DataDirVolume, MountPath: k8sutil.DataDir},
			k8sutil.ConfigOverrideMount(),
		},
		Env: []v1.EnvVar{
			{Name: "ROOK_RGW_KEYRING", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: c.instanceName()}, Key: keyringName}}},
			k8sutil.PodIPEnvVar(k8sutil.PrivateIPEnvVar),
			k8sutil.PodIPEnvVar(k8sutil.PublicIPEnvVar),
			opmon.ClusterNameEnvVar(c.Namespace),
			opmon.EndpointEnvVar(),
			opmon.SecretEnvVar(),
			opmon.AdminSecretEnvVar(),
			k8sutil.ConfigOverrideEnvVar(),
		},
	}
}

func (c *Cluster) startService() (string, error) {
	labels := c.getLabels()
	s := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.instanceName(),
			Namespace: c.Namespace,
			Labels:    labels,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       c.instanceName(),
					Port:       cephrgw.RGWPort,
					TargetPort: intstr.FromInt(int(cephrgw.RGWPort)),
					Protocol:   v1.ProtocolTCP,
				},
			},
			Selector: labels,
		},
	}

	s, err := c.context.Clientset.CoreV1().Services(c.Namespace).Create(s)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", fmt.Errorf("failed to create mon service. %+v", err)
		}
		logger.Infof("RGW service already running")
		return "", nil
	}

	logger.Infof("RGW service running at %s:%d", s.Spec.ClusterIP, cephrgw.RGWPort)
	return s.Spec.ClusterIP, nil
}

func (c *Cluster) getLabels() map[string]string {
	return map[string]string{
		k8sutil.AppAttr:     appName,
		k8sutil.ClusterAttr: c.Namespace,
		"rook_object_store": c.Name,
	}
}

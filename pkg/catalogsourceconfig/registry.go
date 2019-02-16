package catalogsourceconfig

import (
	"context"
	"strconv"
	"strings"

	"github.com/operator-framework/operator-marketplace/pkg/apis/marketplace/v1alpha1"
	"github.com/operator-framework/operator-marketplace/pkg/datastore"
	"github.com/sirupsen/logrus"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	containerName   = "registry-server"
	clusterRoleName = "marketplace-operator-registry-server"
	portNumber      = 50051
	portName        = "grpc"
)

var action = []string{"grpc_health_probe", "-addr=localhost:50051"}

type catalogSourceConfigWrapper struct {
	*v1alpha1.CatalogSourceConfig
}

func (c *catalogSourceConfigWrapper) key() client.ObjectKey {
	return client.ObjectKey{
		Name:      c.GetName(),
		Namespace: c.GetNamespace(),
	}
}

type registry struct {
	log     *logrus.Entry
	client  client.Client
	reader  datastore.Reader
	csc     catalogSourceConfigWrapper
	image   string
	address string
}

// Registry contains the method that ensures a registry-pod deployment and its
// associated resources are created.
type Registry interface {
	Ensure() error
	GetAddress() string
}

// NewRegistry returns an initialized instance of Registry
func NewRegistry(log *logrus.Entry, client client.Client, reader datastore.Reader, csc *v1alpha1.CatalogSourceConfig, image string) Registry {
	return &registry{
		log:    log,
		client: client,
		reader: reader,
		csc:    catalogSourceConfigWrapper{csc},
		image:  image,
	}
}

// Ensure ensures a registry-pod deployment and its associated
// resources are created.
func (r *registry) Ensure() error {
	oprsrcString, oprsrcList := r.getOperatorSources()

	if err := r.ensureServiceAccount(); err != nil {
		return err
	}
	if err := r.ensureRole(oprsrcList); err != nil {
		return err
	}
	if err := r.ensureRoleBinding(); err != nil {
		return err
	}
	if err := r.ensureDeployment(oprsrcString); err != nil {
		return err
	}
	if err := r.ensureService(); err != nil {
		return err
	}
	return nil
}

func (r *registry) GetAddress() string {
	return r.address
}

// ensureDeployment ensures that registry Deployment is present that for serving
// a database consisting of the packages from the given operatorSources
func (r *registry) ensureDeployment(operatorSources string) error {
	registryCommand := getCommand(r.csc.Spec.Packages, operatorSources)
	deployment := new(DeploymentBuilder).WithTypeMeta().Deployment()
	if err := r.client.Get(context.TODO(), r.csc.key(), deployment); err != nil {
		deployment = r.newDeployment(registryCommand)
		err = r.client.Create(context.TODO(), deployment)
		if err != nil && !errors.IsAlreadyExists(err) {
			r.log.Errorf("Failed to create Deployment %s: %v", deployment.GetName(), err)
			return err
		}
		r.log.Infof("Created Deployment %s with registry command: %s", deployment.GetName(), registryCommand)
	} else {
		// Update the pod specification
		deployment.Spec.Template = r.newPodTemplateSpec(registryCommand)
		err = r.client.Update(context.TODO(), deployment)
		if err != nil {
			r.log.Errorf("Failed to update Deployment %s : %v", deployment.GetName(), err)
			return err
		}
		r.log.Infof("Updated Deployment %s with registry command: %s", deployment.GetName(), registryCommand)
	}
	return nil
}

// ensureRole ensure that the Role required to access the given operatorSources
// from the registry Deployment is present.
func (r *registry) ensureRole(operatorSources []string) error {
	role := new(RoleBuilder).WithTypeMeta().Role()
	if err := r.client.Get(context.TODO(), r.csc.key(), role); err != nil {
		role = r.newRole(operatorSources)
		err = r.client.Create(context.TODO(), role)
		if err != nil && !errors.IsAlreadyExists(err) {
			r.log.Errorf("Failed to create Role %s: %v", role.GetName(), err)
			return err
		}
		r.log.Infof("Created Role %s", role.GetName())
	} else {
		// Update the Rules to be on the safe side
		role.Rules = getRules(operatorSources)
		err = r.client.Update(context.TODO(), role)
		if err != nil {
			r.log.Errorf("Failed to update Role %s : %v", role.GetName(), err)
			return err
		}
		r.log.Infof("Updated Role %s", role.GetName())
	}
	return nil
}

// ensureRoleBinding ensures that the RoleBinding bound to the Role previously
// created is present.
func (r *registry) ensureRoleBinding() error {
	roleBinding := new(RoleBindingBuilder).WithTypeMeta().RoleBinding()
	if err := r.client.Get(context.TODO(), r.csc.key(), roleBinding); err != nil {
		roleBinding = r.newRoleBinding(r.csc.GetName())
		err = r.client.Create(context.TODO(), roleBinding)
		if err != nil && !errors.IsAlreadyExists(err) {
			r.log.Errorf("Failed to create RoleBinding %s: %v", roleBinding.GetName(), err)
			return err
		}
		r.log.Infof("Created RoleBinding %s", roleBinding.GetName())
	} else {
		// Update the Rules to be on the safe side
		roleBinding.RoleRef = NewRoleRef(r.csc.GetName())
		err = r.client.Update(context.TODO(), roleBinding)
		if err != nil {
			r.log.Errorf("Failed to update RoleBinding %s : %v", roleBinding.GetName(), err)
			return err
		}
		r.log.Infof("Updated RoleBinding %s", roleBinding.GetName())
	}
	return nil
}

// ensureService ensure that the Service for the registry deployment is present.
func (r *registry) ensureService() error {
	service := new(ServiceBuilder).WithTypeMeta().Service()
	// Delete the Service so that we get a new ClusterIP
	if err := r.client.Get(context.TODO(), r.csc.key(), service); err == nil {
		r.log.Infof("Service %s is present", service.GetName())
		err := r.client.Delete(context.TODO(), service)
		if err != nil {
			r.log.Errorf("Failed to delete Service %s", service.GetName())
			// Make a best effort to create the service
		} else {
			r.log.Infof("Deleted Service %s", service.GetName())
		}
	}
	service = r.newService()
	if err := r.client.Create(context.TODO(), service); err != nil && !errors.IsAlreadyExists(err) {
		r.log.Errorf("Failed to create Service %s: %v", service.GetName(), err)
		return err
	}
	r.log.Infof("Created Service %s", service.GetName())

	r.address = service.Spec.ClusterIP + ":" + strconv.Itoa(int(service.Spec.Ports[0].Port))
	return nil
}

// ensureServiceAccount ensure that the ServiceAccount required to be associated
// with the Deployment is present.
func (r *registry) ensureServiceAccount() error {
	serviceAccount := new(ServiceAccountBuilder).WithTypeMeta().ServiceAccount()
	if err := r.client.Get(context.TODO(), r.csc.key(), serviceAccount); err != nil {
		serviceAccount = r.newServiceAccount()
		err = r.client.Create(context.TODO(), serviceAccount)
		if err != nil && !errors.IsAlreadyExists(err) {
			r.log.Errorf("Failed to create ServiceAccount %s: %v", serviceAccount.GetName(), err)
			return err
		}
		r.log.Infof("Created ServiceAccount %s", serviceAccount.GetName())
	} else {
		r.log.Infof("ServiceAccount %s is present", serviceAccount.GetName())
	}
	return nil
}

// getLabels returns the label that must match between the Deployment's
// LabelSelector and the Pod template's label
func (r *registry) getLabel() map[string]string {
	return map[string]string{"marketplace.catalogSourceConfig": r.csc.GetName()}
}

// getOperatorSources returns a concatanated string of namespaced OperatorSources
// and an array of OperatorSource names where the packages in the
// CatalogSourceConfig are present.
func (r *registry) getOperatorSources() (string, []string) {
	var opsrcString string
	var opsrcList []string
	for _, packageID := range GetPackageIDs(r.csc.Spec.Packages) {
		opsrcMeta, err := r.reader.Read(packageID)
		if err != nil {
			r.log.Errorf("Error %v reading package %s", err, packageID)
			continue
		}
		opsrcNamespacedName := opsrcMeta.Namespace + "/" + opsrcMeta.Name
		if !strings.Contains(opsrcString, opsrcNamespacedName) {
			opsrcString += opsrcNamespacedName + ","
			opsrcList = append(opsrcList, opsrcMeta.Name)
		}
	}
	opsrcString = strings.TrimSuffix(opsrcString, ",")
	return opsrcString, opsrcList
}

// getSubjects returns the Subjects that the RoleBinding should apply to.
func (r *registry) getSubjects() []rbac.Subject {
	return []rbac.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      r.csc.GetName(),
			Namespace: r.csc.GetNamespace(),
		},
	}
}

// newDeployment() returns a Deployment object that can be used to bring up a
// registry deployment
func (r *registry) newDeployment(registryCommand []string) *apps.Deployment {
	return new(DeploymentBuilder).
		WithMeta(r.csc.GetName(), r.csc.GetNamespace()).
		WithOwner(r.csc.CatalogSourceConfig).
		WithSpec(1, r.getLabel(), r.newPodTemplateSpec(registryCommand)).
		Deployment()
}

// newPodTemplateSpec returns a PodTemplateSpec object that can be used to bring
// up a registry pod
func (r *registry) newPodTemplateSpec(registryCommand []string) core.PodTemplateSpec {
	return core.PodTemplateSpec{
		ObjectMeta: meta.ObjectMeta{
			Name:      r.csc.GetName(),
			Namespace: r.csc.GetNamespace(),
			Labels:    r.getLabel(),
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{
					Name:    r.csc.GetName(),
					Image:   r.image,
					Command: registryCommand,
					Ports: []core.ContainerPort{
						{
							Name:          portName,
							ContainerPort: portNumber,
						},
					},
					ReadinessProbe: &core.Probe{
						Handler: core.Handler{
							Exec: &core.ExecAction{
								Command: action,
							},
						},
						InitialDelaySeconds: 5,
						FailureThreshold:    30,
					},
					LivenessProbe: &core.Probe{
						Handler: core.Handler{
							Exec: &core.ExecAction{
								Command: action,
							},
						},
						InitialDelaySeconds: 5,
						FailureThreshold:    30,
					},
				},
			},
			ServiceAccountName: r.csc.GetName(),
		},
	}
}

// newRole returns a Role object with the rules set to access the given
// operatorSources from the registry pod
func (r *registry) newRole(operatorSources []string) *rbac.Role {
	return new(RoleBuilder).
		WithMeta(r.csc.GetName(), r.csc.GetNamespace()).
		WithOwner(r.csc.CatalogSourceConfig).
		WithRules(getRules(operatorSources)).
		Role()
}

// newRoleBinding returns a RoleBinding object RoleRef set to the given Role.
func (r *registry) newRoleBinding(roleName string) *rbac.RoleBinding {
	return new(RoleBindingBuilder).
		WithMeta(r.csc.GetName(), r.csc.GetNamespace()).
		WithOwner(r.csc.CatalogSourceConfig).
		WithSubjects(r.getSubjects()).
		WithRoleRef(roleName).
		RoleBinding()
}

// newService returns a new Service object.
func (r *registry) newService() *core.Service {
	return new(ServiceBuilder).
		WithMeta(r.csc.GetName(), r.csc.GetNamespace()).
		WithOwner(r.csc.CatalogSourceConfig).
		WithSpec(r.newServiceSpec()).
		Service()
}

// newServiceAccount returns a new ServiceAccount object.
func (r *registry) newServiceAccount() *core.ServiceAccount {
	return new(ServiceAccountBuilder).
		WithMeta(r.csc.GetName(), r.csc.GetNamespace()).
		WithOwner(r.csc.CatalogSourceConfig).
		ServiceAccount()
}

// newServiceSpec returns a ServiceSpec as required to front the registry deployment
func (r *registry) newServiceSpec() core.ServiceSpec {
	return core.ServiceSpec{
		Ports: []core.ServicePort{
			{
				Name:       portName,
				Port:       portNumber,
				TargetPort: intstr.FromInt(portNumber),
			},
		},
		Selector: r.getLabel(),
	}
}

// getCommand returns the command used to launch the registry server
func getCommand(packages string, sources string) []string {
	return []string{"appregistry-server", "-s", sources, "-o", packages}
}

// getRules returns the PolicyRule needed to access the given operatorSources and secrets
// from the registry pod
func getRules(operatorSources []string) []rbac.PolicyRule {
	return []rbac.PolicyRule{
		NewRule([]string{"get"}, []string{"marketplace.redhat.com"}, []string{"operatorsources"}, operatorSources),
		NewRule([]string{"get"}, []string{""}, []string{"secrets"}, nil),
	}
}

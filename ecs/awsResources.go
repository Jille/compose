/*
   Copyright 2020 Docker Compose CLI authors

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

package ecs

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/compose-cli/api/compose"
	"github.com/docker/compose-cli/errdefs"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/awslabs/goformation/v4/cloudformation"
	"github.com/awslabs/goformation/v4/cloudformation/ec2"
	"github.com/awslabs/goformation/v4/cloudformation/ecs"
	"github.com/awslabs/goformation/v4/cloudformation/efs"
	"github.com/awslabs/goformation/v4/cloudformation/elasticloadbalancingv2"
	"github.com/compose-spec/compose-go/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// awsResources hold the AWS component being used or created to support services definition
type awsResources struct {
	vpc              string
	subnets          []string
	cluster          string
	loadBalancer     string
	loadBalancerType string
	securityGroups   map[string]string
	filesystems      map[string]string
}

func (r *awsResources) serviceSecurityGroups(service types.ServiceConfig) []string {
	var groups []string
	for net := range service.Networks {
		groups = append(groups, r.securityGroups[net])
	}
	return groups
}

func (r *awsResources) allSecurityGroups() []string {
	var securityGroups []string
	for _, r := range r.securityGroups {
		securityGroups = append(securityGroups, r)
	}
	return securityGroups
}

// parse look into compose project for configured resource to use, and check they are valid
func (b *ecsAPIService) parse(ctx context.Context, project *types.Project) (awsResources, error) {
	r := awsResources{}
	var err error
	r.cluster, err = b.parseClusterExtension(ctx, project)
	if err != nil {
		return r, err
	}
	r.vpc, r.subnets, err = b.parseVPCExtension(ctx, project)
	if err != nil {
		return r, err
	}
	r.loadBalancer, r.loadBalancerType, err = b.parseLoadBalancerExtension(ctx, project)
	if err != nil {
		return r, err
	}
	r.securityGroups, err = b.parseExternalNetworks(ctx, project)
	if err != nil {
		return r, err
	}
	r.filesystems, err = b.parseExternalVolumes(ctx, project)
	if err != nil {
		return r, err
	}
	return r, nil
}

func (b *ecsAPIService) parseClusterExtension(ctx context.Context, project *types.Project) (string, error) {
	if x, ok := project.Extensions[extensionCluster]; ok {
		cluster := x.(string)
		ok, err := b.aws.ClusterExists(ctx, cluster)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.Wrapf(errdefs.ErrNotFound, "cluster %q does not exist", cluster)
		}
		return cluster, nil
	}
	return "", nil
}

func (b *ecsAPIService) parseVPCExtension(ctx context.Context, project *types.Project) (string, []string, error) {
	var vpc string
	if x, ok := project.Extensions[extensionVPC]; ok {
		vpc = x.(string)
		err := b.aws.CheckVPC(ctx, vpc)
		if err != nil {
			return "", nil, err
		}

	} else {
		defaultVPC, err := b.aws.GetDefaultVPC(ctx)
		if err != nil {
			return "", nil, err
		}
		vpc = defaultVPC
	}

	subNets, err := b.aws.GetSubNets(ctx, vpc)
	if err != nil {
		return "", nil, err
	}
	if len(subNets) < 2 {
		return "", nil, fmt.Errorf("VPC %s should have at least 2 associated subnets in different availability zones", vpc)
	}
	return vpc, subNets, nil
}

func (b *ecsAPIService) parseLoadBalancerExtension(ctx context.Context, project *types.Project) (string, string, error) {
	if x, ok := project.Extensions[extensionLoadBalancer]; ok {
		loadBalancer := x.(string)
		loadBalancerType, err := b.aws.LoadBalancerType(ctx, loadBalancer)
		if err != nil {
			return "", "", err
		}

		required := getRequiredLoadBalancerType(project)
		if loadBalancerType != required {
			return "", "", fmt.Errorf("load balancer %s is of type %s, project require a %s", loadBalancer, loadBalancerType, required)
		}

		return loadBalancer, loadBalancerType, nil
	}
	return "", "", nil
}

func (b *ecsAPIService) parseExternalNetworks(ctx context.Context, project *types.Project) (map[string]string, error) {
	securityGroups := make(map[string]string, len(project.Networks))
	for name, net := range project.Networks {
		// FIXME remove this for G.A
		if x, ok := net.Extensions[extensionSecurityGroup]; ok {
			logrus.Warn("to use an existing security-group, use `network.external` and `network.name` in your compose file")
			logrus.Debugf("Security Group for network %q set by user to %q", net.Name, x)
			net.External.External = true
			net.Name = x.(string)
			project.Networks[name] = net
		}

		if !net.External.External {
			continue
		}
		exists, err := b.aws.SecurityGroupExists(ctx, net.Name)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, errors.Wrapf(errdefs.ErrNotFound, "security group %q doesn't exist", net.Name)
		}
		securityGroups[name] = net.Name
	}
	return securityGroups, nil
}

func (b *ecsAPIService) parseExternalVolumes(ctx context.Context, project *types.Project) (map[string]string, error) {
	filesystems := make(map[string]string, len(project.Volumes))
	for name, vol := range project.Volumes {
		if vol.External.External {
			exists, err := b.aws.FileSystemExists(ctx, vol.Name)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, errors.Wrapf(errdefs.ErrNotFound, "EFS file system %q doesn't exist", vol.Name)
			}
			filesystems[name] = vol.Name
			continue
		}

		logrus.Debugf("searching for existing filesystem as volume %q", name)
		tags := map[string]string{
			compose.ProjectTag: project.Name,
			compose.VolumeTag:  name,
		}
		id, err := b.aws.FindFileSystem(ctx, tags)
		if err != nil {
			return nil, err
		}
		if id != "" {
			filesystems[name] = id
		}
	}
	return filesystems, nil
}

// ensureResources create required resources in template if not yet defined
func (b *ecsAPIService) ensureResources(resources *awsResources, project *types.Project, template *cloudformation.Template) error {
	b.ensureCluster(resources, project, template)
	b.ensureNetworks(resources, project, template)
	err := b.ensureVolumes(resources, project, template)
	if err != nil {
		return err
	}
	b.ensureLoadBalancer(resources, project, template)
	return nil
}

func (b *ecsAPIService) ensureCluster(r *awsResources, project *types.Project, template *cloudformation.Template) {
	if r.cluster != "" {
		return
	}
	template.Resources["Cluster"] = &ecs.Cluster{
		ClusterName: project.Name,
		Tags:        projectTags(project),
	}
	r.cluster = cloudformation.Ref("Cluster")
}

func (b *ecsAPIService) ensureNetworks(r *awsResources, project *types.Project, template *cloudformation.Template) {
	if r.securityGroups == nil {
		r.securityGroups = make(map[string]string, len(project.Networks))
	}
	for name, net := range project.Networks {
		if _, ok := r.securityGroups[name]; ok {
			continue
		}
		securityGroup := networkResourceName(name)
		template.Resources[securityGroup] = &ec2.SecurityGroup{
			GroupDescription: fmt.Sprintf("%s Security Group for %s network", project.Name, name),
			VpcId:            r.vpc,
			Tags:             networkTags(project, net),
		}

		ingress := securityGroup + "Ingress"
		template.Resources[ingress] = &ec2.SecurityGroupIngress{
			Description:           fmt.Sprintf("Allow communication within network %s", name),
			IpProtocol:            allProtocols,
			GroupId:               cloudformation.Ref(securityGroup),
			SourceSecurityGroupId: cloudformation.Ref(securityGroup),
		}

		r.securityGroups[name] = cloudformation.Ref(securityGroup)
	}
}

func (b *ecsAPIService) ensureVolumes(r *awsResources, project *types.Project, template *cloudformation.Template) error {
	for name, volume := range project.Volumes {
		if _, ok := r.filesystems[name]; ok {
			continue
		}

		var backupPolicy *efs.FileSystem_BackupPolicy
		if backup, ok := volume.DriverOpts["backup_policy"]; ok {
			backupPolicy = &efs.FileSystem_BackupPolicy{
				Status: backup,
			}
		}

		var lifecyclePolicies []efs.FileSystem_LifecyclePolicy
		if policy, ok := volume.DriverOpts["lifecycle_policy"]; ok {
			lifecyclePolicies = append(lifecyclePolicies, efs.FileSystem_LifecyclePolicy{
				TransitionToIA: strings.TrimSpace(policy),
			})
		}

		var provisionedThroughputInMibps float64
		if t, ok := volume.DriverOpts["provisioned_throughput"]; ok {
			v, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return err
			}
			provisionedThroughputInMibps = v
		}

		var performanceMode = volume.DriverOpts["performance_mode"]
		var throughputMode = volume.DriverOpts["throughput_mode"]
		var kmsKeyID = volume.DriverOpts["kms_key_id"]

		n := volumeResourceName(name)
		template.Resources[n] = &efs.FileSystem{
			BackupPolicy:     backupPolicy,
			Encrypted:        true,
			FileSystemPolicy: nil,
			FileSystemTags: []efs.FileSystem_ElasticFileSystemTag{
				{
					Key:   compose.ProjectTag,
					Value: project.Name,
				},
				{
					Key:   compose.VolumeTag,
					Value: name,
				},
				{
					Key:   "Name",
					Value: fmt.Sprintf("%s_%s", project.Name, name),
				},
			},
			KmsKeyId:                        kmsKeyID,
			LifecyclePolicies:               lifecyclePolicies,
			PerformanceMode:                 performanceMode,
			ProvisionedThroughputInMibps:    provisionedThroughputInMibps,
			ThroughputMode:                  throughputMode,
			AWSCloudFormationDeletionPolicy: "Retain",
		}
		r.filesystems[name] = cloudformation.Ref(n)
	}
	return nil
}

func (b *ecsAPIService) ensureLoadBalancer(r *awsResources, project *types.Project, template *cloudformation.Template) {
	if r.loadBalancer != "" {
		return
	}
	if allServices(project.Services, func(it types.ServiceConfig) bool {
		return len(it.Ports) == 0
	}) {
		logrus.Debug("Application does not expose any public port, so no need for a LoadBalancer")
		return
	}

	balancerType := getRequiredLoadBalancerType(project)
	var securityGroups []string
	if balancerType == elbv2.LoadBalancerTypeEnumApplication {
		// see https://docs.aws.amazon.com/elasticloadbalancing/latest/network/target-group-register-targets.html#target-security-groups
		// Network Load Balancers do not have associated security groups
		securityGroups = r.getLoadBalancerSecurityGroups(project)
	}
	template.Resources["LoadBalancer"] = &elasticloadbalancingv2.LoadBalancer{
		Scheme:         elbv2.LoadBalancerSchemeEnumInternetFacing,
		SecurityGroups: securityGroups,
		Subnets:        r.subnets,
		Tags:           projectTags(project),
		Type:           balancerType,
	}
	r.loadBalancer = cloudformation.Ref("LoadBalancer")
	r.loadBalancerType = balancerType
}

func (r *awsResources) getLoadBalancerSecurityGroups(project *types.Project) []string {
	securityGroups := []string{}
	for name, network := range project.Networks {
		if !network.Internal {
			securityGroups = append(securityGroups, r.securityGroups[name])
		}
	}
	return securityGroups
}

func getRequiredLoadBalancerType(project *types.Project) string {
	loadBalancerType := elbv2.LoadBalancerTypeEnumNetwork
	if allServices(project.Services, func(it types.ServiceConfig) bool {
		return allPorts(it.Ports, portIsHTTP)
	}) {
		loadBalancerType = elbv2.LoadBalancerTypeEnumApplication
	}
	return loadBalancerType
}

func portIsHTTP(it types.ServicePortConfig) bool {
	if v, ok := it.Extensions[extensionProtocol]; ok {
		protocol := v.(string)
		return protocol == "http" || protocol == "https"
	}
	return it.Target == 80 || it.Target == 443
}

// predicate[types.ServiceConfig]
type servicePredicate func(it types.ServiceConfig) bool

// all[types.ServiceConfig]
func allServices(services types.Services, p servicePredicate) bool {
	for _, s := range services {
		if !p(s) {
			return false
		}
	}
	return true
}

// predicate[types.ServicePortConfig]
type portPredicate func(it types.ServicePortConfig) bool

// all[types.ServicePortConfig]
func allPorts(ports []types.ServicePortConfig, p portPredicate) bool {
	for _, s := range ports {
		if !p(s) {
			return false
		}
	}
	return true
}

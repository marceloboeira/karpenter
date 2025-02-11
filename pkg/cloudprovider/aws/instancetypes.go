/*
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

package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/awslabs/karpenter/pkg/cloudprovider/aws/utils"
	"github.com/awslabs/karpenter/pkg/packing"
	"github.com/awslabs/karpenter/pkg/utils/functional"
	"github.com/awslabs/karpenter/pkg/utils/resources"
	"github.com/patrickmn/go-cache"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
)

const (
	allInstanceTypesKey = "all"
)

type InstanceTypeProvider struct {
	ec2api ec2iface.EC2API
	cache  *cache.Cache
}

func NewInstanceTypeProvider(ec2api ec2iface.EC2API) *InstanceTypeProvider {
	return &InstanceTypeProvider{
		ec2api: ec2api,
		cache:  cache.New(CacheTTL, CacheCleanupInterval),
	}
}

// Get instance types that are availble per availability zone
func (p *InstanceTypeProvider) Get(ctx context.Context, zonalSubnetOptions map[string][]*ec2.Subnet, constraints Constraints) ([]*packing.Instance, error) {
	zones := []string{}
	for zone := range zonalSubnetOptions {
		zones = append(zones, zone)
	}

	var supportedInstanceTypes []*packing.Instance
	if instanceTypes, ok := p.cache.Get(allInstanceTypesKey); ok {
		supportedInstanceTypes = instanceTypes.([]*packing.Instance)
	} else {
		var err error
		supportedInstanceTypes, err = p.getZonalInstanceTypes(ctx)
		if err != nil {
			return nil, err
		}
		p.cache.SetDefault(allInstanceTypesKey, supportedInstanceTypes)
		zap.S().Debugf("Successfully discovered %d EC2 instance types", len(supportedInstanceTypes))
	}
	return p.filterFrom(supportedInstanceTypes, constraints, zones), nil
}

// GetAllInstanceTypeNames returns all instance type names without filtering based on constraints
func (p *InstanceTypeProvider) GetAllInstanceTypeNames(ctx context.Context) ([]string, error) {
	supportedInstanceTypes, err := p.Get(ctx, map[string][]*ec2.Subnet{}, Constraints{})
	if err != nil {
		return nil, err
	}
	instanceTypeNames := []string{}
	for _, instanceType := range supportedInstanceTypes {
		instanceTypeNames = append(instanceTypeNames, *instanceType.InstanceType)
	}
	return instanceTypeNames, nil
}

func (p *InstanceTypeProvider) getZonalInstanceTypes(ctx context.Context) ([]*packing.Instance, error) {
	instanceTypes, err := p.getAllInstanceTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieving all instance types, %w", err)
	}

	inputs := &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String("availability-zone"),
	}

	zonalInstanceTypeNames := map[string][]string{}
	err = p.ec2api.DescribeInstanceTypeOfferingsPagesWithContext(ctx, inputs, func(output *ec2.DescribeInstanceTypeOfferingsOutput, lastPage bool) bool {
		for _, offerings := range output.InstanceTypeOfferings {
			zonalInstanceTypeNames[*offerings.Location] = append(zonalInstanceTypeNames[*offerings.Location], *offerings.InstanceType)
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("describing instance type zone offerings, %w", err)
	}

	// aggregate supported zones into each instance type
	ec2InstanceTypes := map[string]*packing.Instance{}
	supportedInstanceTypes := []*packing.Instance{}
	for _, instanceTypeInfo := range instanceTypes {
		for zone, instanceTypeNames := range zonalInstanceTypeNames {
			for _, instanceTypeName := range instanceTypeNames {
				if instanceTypeName == *instanceTypeInfo.InstanceType {
					if it, ok := ec2InstanceTypes[instanceTypeName]; ok {
						it.Zones = append(it.Zones, zone)
					} else {
						instanceType := &packing.Instance{InstanceTypeInfo: *instanceTypeInfo, Zones: []string{zone}}
						supportedInstanceTypes = append(supportedInstanceTypes, instanceType)
						ec2InstanceTypes[instanceTypeName] = instanceType
					}
				}
			}
		}
	}
	return supportedInstanceTypes, nil
}

// getAllInstanceTypes retrieves all instance types from the ec2 DescribeInstanceTypes API using some opinionated filters
func (p *InstanceTypeProvider) getAllInstanceTypes(ctx context.Context) ([]*ec2.InstanceTypeInfo, error) {
	instanceTypes := []*ec2.InstanceTypeInfo{}
	describeInstanceTypesInput := &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("supported-virtualization-type"),
				Values: []*string{aws.String("hvm")},
			},
		},
	}
	err := p.ec2api.DescribeInstanceTypesPagesWithContext(ctx, describeInstanceTypesInput, func(page *ec2.DescribeInstanceTypesOutput, lastPage bool) bool {
		instanceTypes = append(instanceTypes, page.InstanceTypes...)
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("fetching instance types using ec2.DescribeInstanceTypes, %w", err)
	}
	return instanceTypes, nil
}

// filterFrom returns a filtered list of instance types based on the provided resource constraints
func (p *InstanceTypeProvider) filterFrom(instanceTypes []*packing.Instance, constraints Constraints, zones []string) []*packing.Instance {
	filtered := []*packing.Instance{}
	for _, instanceTypeInfo := range instanceTypes {
		requests := resources.RequestsForPods(constraints.Pods...)
		if p.isInstanceTypeSupported(constraints.InstanceTypes, instanceTypeInfo) &&
			p.isCapacityTypeSupported(constraints.GetCapacityType(), instanceTypeInfo) &&
			p.isArchitectureSupported(utils.NormalizeArchitecture(constraints.Architecture), instanceTypeInfo) &&
			p.isZonesSupported(zones, instanceTypeInfo) &&
			p.isNvidiaGPUSupported(requests, instanceTypeInfo) &&
			p.isAWSNeuronSupported(requests, instanceTypeInfo) {
			filtered = append(filtered, instanceTypeInfo)
		}
	}
	return filtered
}

func (p *InstanceTypeProvider) isInstanceTypeSupported(instanceTypeConstraints []string, instance *packing.Instance) bool {
	if len(instanceTypeConstraints) == 0 && p.isDefaultInstanceType(instance) {
		return true
	}
	if len(instanceTypeConstraints) != 0 && functional.ContainsString(instanceTypeConstraints, *instance.InstanceType) {
		return true
	}
	return false
}

// isDefaultInstanceType returns true if the instance type provided conforms to the default instance type criteria
// This function is used to make sure we launch instance types that are suited for general workloads
func (p *InstanceTypeProvider) isDefaultInstanceType(instanceTypeInfo *packing.Instance) bool {
	return instanceTypeInfo.FpgaInfo == nil &&
		!*instanceTypeInfo.BareMetal &&
		functional.HasAnyPrefix(*instanceTypeInfo.InstanceType,
			"m", "c", "r", "a", // Standard
			"t3", "t4", // Burstable
			"p", "inf", "g", // Accelerators
		)
}

func (p *InstanceTypeProvider) isArchitectureSupported(architecture *string, instance *packing.Instance) bool {
	return architecture == nil ||
		functional.ContainsString(aws.StringValueSlice(instance.ProcessorInfo.SupportedArchitectures), *architecture)
}

func (p *InstanceTypeProvider) isCapacityTypeSupported(capacityType string, instance *packing.Instance) bool {
	return capacityType == "" ||
		functional.ContainsString(aws.StringValueSlice(instance.SupportedUsageClasses), capacityType)
}

func (p *InstanceTypeProvider) isNvidiaGPUSupported(requests v1.ResourceList, instanceTypeInfo *packing.Instance) bool {
	if _, ok := requests[resources.NvidiaGPU]; ok {
		return instanceTypeInfo.GpuInfo != nil && *instanceTypeInfo.GpuInfo.Gpus[0].Manufacturer == "NVIDIA"
	}
	return true
}
func (p *InstanceTypeProvider) isAWSNeuronSupported(requests v1.ResourceList, instanceTypeInfo *packing.Instance) bool {
	if _, ok := requests[resources.AWSNeuron]; ok {
		return instanceTypeInfo.InferenceAcceleratorInfo != nil && *instanceTypeInfo.InferenceAcceleratorInfo.Accelerators[0].Manufacturer == "AWS"
	}
	return true
}

func (p *InstanceTypeProvider) isZonesSupported(zones []string, instance *packing.Instance) bool {
	return len(zones) == 0 || len(functional.IntersectStringSlice(instance.Zones, zones)) > 0
}

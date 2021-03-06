package lepton

import (
	"encoding/base64"
	"errors"

	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ebs"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/route53"

	"github.com/olekukonko/tablewriter"
)

// AWS contains all operations for AWS
type AWS struct {
	Storage       *S3
	dnsService    *route53.Route53
	volumeService *ebs.EBS
}

// BuildImage to be upload on AWS
func (p *AWS) BuildImage(ctx *Context) (string, error) {
	c := ctx.config
	err := BuildImage(*c)
	if err != nil {
		return "", err
	}

	return p.customizeImage(ctx)
}

// BuildImageWithPackage to upload on AWS
func (p *AWS) BuildImageWithPackage(ctx *Context, pkgpath string) (string, error) {
	c := ctx.config
	err := BuildImageFromPackage(pkgpath, *c)
	if err != nil {
		return "", err
	}
	return p.customizeImage(ctx)
}

// Initialize AWS related things
func (p *AWS) Initialize() error {
	p.Storage = &S3{}

	return nil
}

func (p *AWS) getDNSService(config *Config) (*route53.Route53, error) {
	if p.dnsService != nil {
		return p.dnsService, nil
	}

	sess, err := p.getAWSSession(config)
	if err != nil {
		return nil, err
	}

	p.dnsService = route53.New(sess)

	return p.dnsService, nil
}

// CreateImage - Creates image on AWS using nanos images
// TODO : re-use and cache DefaultClient and instances.
func (p *AWS) CreateImage(ctx *Context) error {
	// this is a really convulted setup
	// 1) upload the image
	// 2) create a snapshot
	// 3) create an image

	compute, err := p.getEc2Service(ctx.config)
	if err != nil {
		return err
	}

	c := ctx.config

	bucket := c.CloudConfig.BucketName
	key := c.CloudConfig.ImageName

	input := &ec2.ImportSnapshotInput{
		Description: aws.String("NanoVMs test"),
		DiskContainer: &ec2.SnapshotDiskContainer{
			Description: aws.String("NanoVMs test"),
			Format:      aws.String("raw"),
			UserBucket: &ec2.UserBucket{
				S3Bucket: aws.String(bucket),
				S3Key:    aws.String(key),
			},
		},
	}

	res, err := compute.ImportSnapshot(input)
	if err != nil {
		return err
	}

	snapshotID, err := p.waitSnapshotToBeReady(c, res.ImportTaskId)
	if err != nil {
		return err
	}

	// delete the tmp s3 image
	err = p.Storage.DeleteFromBucket(c, key)
	if err != nil {
		return err
	}

	// tag the volume
	_, err = compute.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{snapshotID},
		Tags: []*ec2.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String(key),
			},
		},
	})
	if err != nil {
		return err
	}

	t := time.Now().UnixNano()
	s := strconv.FormatInt(t, 10)

	amiName := key + s

	// register ami
	rinput := &ec2.RegisterImageInput{
		Name:         aws.String(amiName),
		Architecture: aws.String("x86_64"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					DeleteOnTermination: aws.Bool(false),
					SnapshotId:          snapshotID,
					VolumeType:          aws.String("gp2"),
				},
			},
		},
		Description:        aws.String(fmt.Sprintf("nanos image %s", key)),
		RootDeviceName:     aws.String("/dev/sda1"),
		VirtualizationType: aws.String("hvm"),
		EnaSupport:         aws.Bool(false),
	}

	resreg, err := compute.RegisterImage(rinput)
	if err != nil {
		return err
	}

	// Add name tag to the created ami
	_, err = compute.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{resreg.ImageId},
		Tags: []*ec2.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String(key),
			},
		},
	})

	return nil
}

func getAWSImages(region string) (*ec2.DescribeImagesOutput, error) {
	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(region)},
	)
	compute := ec2.New(svc)

	input := &ec2.DescribeImagesInput{
		Owners: []*string{
			aws.String("self"),
		},
	}

	result, err := compute.DescribeImages(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				return nil, errors.New(aerr.Error())
			}
		} else {
			return nil, errors.New(err.Error())
		}
	}

	return result, nil
}

func formalizeAWSInstance(instance *ec2.Instance) *CloudInstance {
	instanceName := "unknown"
	for x := 0; x < len(instance.Tags); x++ {
		if aws.StringValue(instance.Tags[x].Key) == "Name" {
			instanceName = aws.StringValue(instance.Tags[x].Value)
		}
	}

	var privateIps, publicIps []string
	for _, ninterface := range instance.NetworkInterfaces {
		privateIps = append(privateIps, aws.StringValue(ninterface.PrivateIpAddress))

		if ninterface.Association != nil && ninterface.Association.PublicIp != nil {
			publicIps = append(publicIps, aws.StringValue(ninterface.Association.PublicIp))
		}
	}

	return &CloudInstance{
		ID:         aws.StringValue(instance.InstanceId),
		Name:       instanceName,
		Status:     aws.StringValue(instance.State.Name),
		Created:    aws.TimeValue(instance.LaunchTime).String(),
		PublicIps:  publicIps,
		PrivateIps: privateIps,
	}
}

func getAWSInstances(region string, filter []*ec2.Filter) []CloudInstance {
	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(region)},
	)
	compute := ec2.New(svc)

	request := ec2.DescribeInstancesInput{
		Filters: filter,
	}
	result, err := compute.DescribeInstances(&request)

	if err != nil {
		exitWithError("invalid region")
	}

	var cinstances []CloudInstance

	for _, reservation := range result.Reservations {

		for i := 0; i < len(reservation.Instances); i++ {
			instance := reservation.Instances[i]

			cinstances = append(cinstances, *formalizeAWSInstance(instance))
		}

	}

	return cinstances
}

// GetImages return all images on AWS
func (p *AWS) GetImages(ctx *Context) ([]CloudImage, error) {
	var cimages []CloudImage

	result, err := getAWSImages(ctx.config.CloudConfig.Zone)
	if err != nil {
		return nil, err
	}

	images := result.Images
	for _, image := range images {
		var name string
		if image.Tags != nil {
			name = aws.StringValue(image.Tags[0].Value)
		} else {
			name = "n/a"
		}

		cimage := CloudImage{
			Name:    name,
			ID:      *image.Name,
			Status:  *image.State,
			Created: *image.CreationDate,
		}

		cimages = append(cimages, cimage)
	}

	return cimages, nil
}

// ListImages lists images on AWS
func (p *AWS) ListImages(ctx *Context) error {
	cimages, err := p.GetImages(ctx)
	if err != nil {
		return err
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Name", "Id", "Status", "Created"})
	table.SetHeaderColor(
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor})
	table.SetRowLine(true)

	for _, image := range cimages {
		var row []string

		row = append(row, image.Name)
		row = append(row, image.ID)
		row = append(row, image.Status)
		row = append(row, image.Created)

		table.Append(row)
	}

	table.Render()

	return nil
}

// StartInstance stops instance from AWS by ami name
func (p *AWS) StartInstance(ctx *Context, instanceID string) error {

	if instanceID == "" {
		exitWithError("Enter Instance ID")
	}

	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(ctx.config.CloudConfig.Zone)},
	)

	compute := ec2.New(svc)

	if err != nil {
		exitWithError("Invalid region")
	}

	input := &ec2.StartInstancesInput{
		InstanceIds: []*string{
			aws.String(instanceID),
		},
	}

	result, err := compute.StartInstances(input)

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				exitWithError(aerr.Message())
			}
		} else {
			exitWithError(aerr.Message())
		}

	}

	if result.StartingInstances[0].InstanceId != nil {
		fmt.Printf("Started instance : %s\n", *result.StartingInstances[0].InstanceId)
	}

	return nil
}

// StopInstance stops instance from AWS by ami name
func (p *AWS) StopInstance(ctx *Context, instanceID string) error {

	if instanceID == "" {
		exitWithError("Enter InstanceID")
	}

	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(ctx.config.CloudConfig.Zone)},
	)

	compute := ec2.New(svc)

	if err != nil {
		exitWithError("Invalid region")
	}

	input := &ec2.StopInstancesInput{
		InstanceIds: []*string{
			aws.String(instanceID),
		},
	}

	result, err := compute.StopInstances(input)

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				exitWithError(aerr.Message())
			}
		} else {
			exitWithError(aerr.Message())
		}

	}

	if result.StoppingInstances[0].InstanceId != nil {
		fmt.Printf("Stopped instance %s", *result.StoppingInstances[0].InstanceId)
	}

	return nil
}

// ResizeImage is not supported on AWS.
func (p *AWS) ResizeImage(ctx *Context, imagename string, hbytes string) error {
	return fmt.Errorf("Operation not supported")
}

// DeleteImage deletes image from AWS by ami name
func (p *AWS) DeleteImage(ctx *Context, imagename string) error {
	// delete ami by ami name
	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(ctx.config.CloudConfig.Zone)},
	)
	compute := ec2.New(svc)

	ec2Filters := []*ec2.Filter{}
	vals := []string{imagename}
	ec2Filters = append(ec2Filters, &ec2.Filter{Name: aws.String("name"), Values: aws.StringSlice(vals)})

	input := &ec2.DescribeImagesInput{
		Filters: ec2Filters,
	}

	result, err := compute.DescribeImages(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				fmt.Println(aerr.Error())
			}
		} else {
			fmt.Println(err.Error())
		}
		return err
	}
	if len(result.Images) == 0 {
		return fmt.Errorf("Error running deregister image operation: image %v not found", imagename)
	}

	amiID := aws.StringValue(result.Images[0].ImageId)
	snapID := aws.StringValue(result.Images[0].BlockDeviceMappings[0].Ebs.SnapshotId)

	// grab snapshotid && grab image id

	params := &ec2.DeregisterImageInput{
		ImageId: aws.String(amiID),
		DryRun:  aws.Bool(false),
	}
	_, err = compute.DeregisterImage(params)
	if err != nil {
		return fmt.Errorf("Error running deregister image operation: %s", err)
	}

	// DeleteSnapshot
	params2 := &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(snapID),
		DryRun:     aws.Bool(false),
	}
	_, err = compute.DeleteSnapshot(params2)
	if err != nil {
		return fmt.Errorf("Error running snapshot delete: %s", err)
	}

	return nil
}

// SyncImage syncs image from provider to another provider
func (p *AWS) SyncImage(config *Config, target Provider, image string) error {
	fmt.Println("not yet implemented")
	return nil
}

// parseToAWSTags converts configuration tags to AWS tags and returns the resource name. The defaultName is overriden if there is a tag with key name
func parseToAWSTags(configTags []Tag, defaultName string) ([]*ec2.Tag, string) {
	tags := []*ec2.Tag{}
	var nameSpecified bool
	name := defaultName

	for _, tag := range configTags {
		tags = append(tags, &ec2.Tag{Key: aws.String(tag.Key), Value: aws.String(tag.Value)})
		if tag.Key == "Name" {
			nameSpecified = true
			name = tag.Value
		}
	}

	if !nameSpecified {
		tags = append(tags, &ec2.Tag{
			Key:   aws.String("Name"),
			Value: aws.String(name),
		})
	}

	return tags, name
}

// CreateInstance - Creates instance on AWS Platform
func (p *AWS) CreateInstance(ctx *Context) error {
	result, err := getAWSImages(ctx.config.CloudConfig.Zone)
	if err != nil {
		exitWithError("Invalid zone")
	}

	imgName := ctx.config.CloudConfig.ImageName

	ami := ""
	var last time.Time
	layout := "2006-01-02T15:04:05.000Z"

	for i := 0; i < len(result.Images); i++ {
		n := ""
		if result.Images[i].Tags != nil {
			n = aws.StringValue(result.Images[i].Tags[0].Value)
		}

		if n != "" && n == imgName {
			ami = aws.StringValue(result.Images[i].ImageId)

			ntime := aws.StringValue(result.Images[i].CreationDate)
			t, err := time.Parse(layout, ntime)
			if err != nil {
				return err
			}

			if last.Before(t) {
				last = t
			}
		}
	}

	if ami == "" {
		return errors.New("can't find ami")
	}

	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(ctx.config.CloudConfig.Zone)},
	)

	// Create EC2 service client
	svc := ec2.New(sess)

	// create security group - could take a potential 'RemotePort' from
	// config.json in future
	vpc, err := p.GetVPC(ctx, svc)
	if err != nil {
		return err
	}

	var sg string

	if ctx.config.RunConfig.SecurityGroup != "" && ctx.config.RunConfig.VPC != "" {
		err = p.CheckValidSecurityGroup(ctx, svc)
		if err != nil {
			return err
		}

		sg = ctx.config.RunConfig.SecurityGroup
	} else {
		sg, err = p.CreateSG(ctx, svc, imgName, *vpc.VpcId)
		if err != nil {
			return err
		}
	}

	subnet, err := p.GetSubnet(ctx, svc, *vpc.VpcId)
	if err != nil {
		return err
	}

	if ctx.config.CloudConfig.Flavor == "" {
		ctx.config.CloudConfig.Flavor = "t2.micro"
	}

	// Create tags to assign to the instance
	tags, tagInstanceName := parseToAWSTags(ctx.config.RunConfig.Tags, imgName+"-"+strconv.Itoa(int(time.Now().Unix())))

	// Specify the details of the instance that you want to create.
	runResult, err := svc.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(ami),
		InstanceType: aws.String(ctx.config.CloudConfig.Flavor),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		SubnetId:     aws.String(*subnet.SubnetId),
		SecurityGroupIds: []*string{
			aws.String(sg),
		},
		TagSpecifications: []*ec2.TagSpecification{
			{ResourceType: aws.String("instance"), Tags: tags},
			{ResourceType: aws.String("volume"), Tags: tags},
		},
	})

	if err != nil {
		fmt.Println("Could not create instance", err)
		return err
	}

	fmt.Println("Created instance", *runResult.Instances[0].InstanceId)

	// create dns zones/records to associate DNS record to instance IP
	if ctx.config.RunConfig.DomainName != "" {
		pollCount := 60
		for pollCount > 0 {
			fmt.Printf(".")
			time.Sleep(2 * time.Second)

			instance, err := p.GetInstanceByID(ctx, tagInstanceName)
			if err != nil {
				pollCount--
				continue
			}

			if len(instance.PublicIps) != 0 {
				err := CreateDNSRecord(ctx.config, instance.PublicIps[0], p)
				if err != nil {
					return err
				}
			}
			return nil
		}
		return fmt.Errorf("\nOperation timed out. No of tries %d", pollCount)
	}

	return nil
}

// CheckValidSecurityGroup checks whether the configuration security group exists and has the configuration VPC assigned
func (p *AWS) CheckValidSecurityGroup(ctx *Context, svc *ec2.EC2) error {
	sg := ctx.config.RunConfig.SecurityGroup
	vpc := ctx.config.RunConfig.VPC

	input := &ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{
			aws.String(sg),
		},
	}

	result, err := svc.DescribeSecurityGroups(input)
	if err != nil {
		return fmt.Errorf("get security group with id '%s': %s", sg, err.Error())
	}

	if len(result.SecurityGroups) == 1 && *result.SecurityGroups[0].VpcId != vpc {
		return fmt.Errorf("vpc mismatch: expected '%s' to have vpc '%s', got '%s'", sg, ctx.config.RunConfig.VPC, *result.SecurityGroups[0].VpcId)
	} else if len(result.SecurityGroups) == 0 {
		return fmt.Errorf("security group '%s' not found", sg)
	}

	return nil
}

// GetSubnet returns a subnet with the context subnet name or the default subnet of vpc passed by argument
func (p *AWS) GetSubnet(ctx *Context, svc *ec2.EC2, vpcID string) (*ec2.Subnet, error) {
	subnetName := ctx.config.RunConfig.Subnet
	var filters []*ec2.Filter

	filters = append(filters, &ec2.Filter{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})})

	if subnetName != "" {
		filters = append(filters, &ec2.Filter{Name: aws.String("subnet-id"), Values: aws.StringSlice([]string{ctx.config.RunConfig.Subnet})})
	}

	input := &ec2.DescribeSubnetsInput{
		Filters: filters,
	}

	result, err := svc.DescribeSubnets(input)
	if err != nil {
		fmt.Printf("Unable to describe subnets, %v\n", err)
		return nil, err
	}

	if len(result.Subnets) == 0 && subnetName != "" {
		return nil, fmt.Errorf("No Subnets with name '%v' found to associate security group with", subnetName)
	} else if len(result.Subnets) == 0 {
		return nil, errors.New("No Subnets found to associate security group with")
	}

	if subnetName != "" {
		for _, subnet := range result.Subnets {
			if *subnet.DefaultForAz == true {
				return subnet, nil
			}
		}
	}

	return result.Subnets[0], nil
}

// GetVPC returns a vpc with the context vpc name or the default vpc
func (p *AWS) GetVPC(ctx *Context, svc *ec2.EC2) (*ec2.Vpc, error) {
	vpcName := ctx.config.RunConfig.VPC
	var vpc *ec2.Vpc
	var input *ec2.DescribeVpcsInput
	if vpcName != "" {
		var filters []*ec2.Filter

		filters = append(filters, &ec2.Filter{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{ctx.config.RunConfig.VPC})})
		input = &ec2.DescribeVpcsInput{
			Filters: filters,
		}
	}

	result, err := svc.DescribeVpcs(input)
	if err != nil {
		return nil, fmt.Errorf("Unable to describe VPCs, %v", err)
	}
	if len(result.Vpcs) == 0 && vpcName != "" {
		return nil, fmt.Errorf("No VPCs with name '%v' found to associate security group with", vpcName)
	} else if len(result.Vpcs) == 0 {
		return nil, errors.New("No VPCs found to associate security group with")
	}

	if vpcName != "" {
		vpc = result.Vpcs[0]
	} else {
		// select default vpc
		for i, s := range result.Vpcs {
			isDefault := *s.IsDefault
			if isDefault == true {
				vpc = result.Vpcs[i]
			}
		}

		// if there is no default VPC select the first vpc of the list
		if vpc == nil {
			vpc = result.Vpcs[0]
		}
	}

	return vpc, nil
}

func (p AWS) buildFirewallRule(protocol string, port int) *ec2.IpPermission {
	var ec2Permission = new(ec2.IpPermission)
	ec2Permission.SetIpProtocol(protocol)
	ec2Permission.SetFromPort(int64(port))
	ec2Permission.SetToPort(int64(port))
	ec2Permission.SetIpRanges([]*ec2.IpRange{
		{CidrIp: aws.String("0.0.0.0/0")},
	})

	return ec2Permission
}

// CreateSG - Create security group
func (p *AWS) CreateSG(ctx *Context, svc *ec2.EC2, imgName string, vpcID string) (string, error) {
	t := time.Now().UnixNano()
	s := strconv.FormatInt(t, 10)

	sgName := imgName + s

	createRes, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("security group for " + imgName),
		VpcId:       aws.String(vpcID),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "InvalidVpcID.NotFound":
				errstr := fmt.Sprintf("Unable to find VPC with ID %q.", vpcID)
				return "", errors.New(errstr)
			case "InvalidGroup.Duplicate":
				errstr := fmt.Sprintf("Security group %q already exists.", imgName)
				return "", errors.New(errstr)
			}
		}
		errstr := fmt.Sprintf("Unable to create security group %q, %v", imgName, err)
		return "", errors.New(errstr)

	}
	fmt.Printf("Created security group %s with VPC %s.\n",
		aws.StringValue(createRes.GroupId), vpcID)

	var ec2Permissions []*ec2.IpPermission

	for _, port := range ctx.config.RunConfig.Ports {
		rule := p.buildFirewallRule("tcp", port)
		ec2Permissions = append(ec2Permissions, rule)
	}

	for _, port := range ctx.config.RunConfig.UDPPorts {
		rule := p.buildFirewallRule("udp", port)
		ec2Permissions = append(ec2Permissions, rule)
	}

	// maybe have these ports specified from config.json in near future
	if len(ec2Permissions) != 0 {
		_, err = svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       createRes.GroupId,
			IpPermissions: ec2Permissions,
		})
		if err != nil {
			errstr := fmt.Sprintf("Unable to set security group %q ingress, %v", imgName, err)
			return "", errors.New(errstr)
		}
	}

	return aws.StringValue(createRes.GroupId), nil
}

// GetInstanceByID returns the instance with the id passed by argument if it exists
func (p *AWS) GetInstanceByID(ctx *Context, id string) (*CloudInstance, error) {
	var filters []*ec2.Filter

	filters = append(filters, &ec2.Filter{Name: aws.String("tag:Name"), Values: aws.StringSlice([]string{id})})

	instances := getAWSInstances(ctx.config.CloudConfig.Zone, filters)

	if len(instances) == 0 {
		return nil, ErrInstanceNotFound(id)
	}

	return &instances[0], nil
}

// GetInstances return all instances on AWS
func (p *AWS) GetInstances(ctx *Context) ([]CloudInstance, error) {
	cinstances := getAWSInstances(ctx.config.CloudConfig.Zone, nil)

	return cinstances, nil
}

// ListInstances lists instances on AWS
func (p *AWS) ListInstances(ctx *Context) error {
	instances, err := p.GetInstances(ctx)
	if err != nil {
		return err
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Name", "Id", "Status", "Created", "Private Ips", "Public Ips"})
	table.SetHeaderColor(
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor})
	table.SetRowLine(true)

	for _, instance := range instances {

		var rows []string

		rows = append(rows, instance.Name)
		rows = append(rows, instance.ID)

		rows = append(rows, instance.Status)
		rows = append(rows, instance.Created)

		rows = append(rows, strings.Join(instance.PrivateIps, ","))
		rows = append(rows, strings.Join(instance.PublicIps, ","))

		table.Append(rows)
	}

	table.Render()

	return nil
}

// DeleteInstance deletes instance from AWS
func (p *AWS) DeleteInstance(ctx *Context, instancename string) error {
	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(ctx.config.CloudConfig.Zone)},
	)
	compute := ec2.New(svc)

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{
			aws.String(instancename),
		},
	}

	_, err = compute.TerminateInstances(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				fmt.Println(aerr.Error())
			}
		} else {
			fmt.Println(err.Error())
		}
		return err
	}

	// kill off any old security group as well

	return nil
}

// PrintInstanceLogs writes instance logs to console
func (p *AWS) PrintInstanceLogs(ctx *Context, instancename string, watch bool) error {
	l, err := p.GetInstanceLogs(ctx, instancename)
	if err != nil {
		return err
	}
	fmt.Printf(l)
	return nil
}

// GetInstanceLogs gets instance related logs
func (p *AWS) GetInstanceLogs(ctx *Context, instancename string) (string, error) {
	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(ctx.config.CloudConfig.Zone)},
	)
	compute := ec2.New(svc)

	// latest set to true is only avail on nitro (c5) instances
	// otherwise last 64k
	input := &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instancename),
	}

	result, err := compute.GetConsoleOutput(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				fmt.Println(aerr.Error())
			}
		} else {
			fmt.Println(err.Error())
		}
		return "", err
	}

	data, err := base64.StdEncoding.DecodeString(aws.StringValue(result.Output))
	if err != nil {
		return "", err
	}

	l := string(data)

	return l, nil
}

func (p *AWS) customizeImage(ctx *Context) (string, error) {
	imagePath := ctx.config.RunConfig.Imagename
	return imagePath, nil
}

// not an archive - just raw disk image
func (p *AWS) getArchiveName(ctx *Context) string {
	imagePath := ctx.config.RunConfig.Imagename
	return imagePath
}

// GetStorage returns storage interface for cloud provider
func (p *AWS) GetStorage() Storage {
	return p.Storage
}

func (p *AWS) getAWSSession(config *Config) (*session.Session, error) {
	return session.NewSession(
		&aws.Config{
			Region: aws.String(config.CloudConfig.Zone)},
	)
}

func (p *AWS) getEc2Service(config *Config) (*ec2.EC2, error) {
	svc, err := session.NewSession(&aws.Config{
		Region: aws.String(config.CloudConfig.Zone)},
	)
	if err != nil {
		return nil, err
	}

	return ec2.New(svc), nil
}

func (p *AWS) waitSnapshotToBeReady(config *Config, importTaskID *string) (*string, error) {
	compute, err := p.getEc2Service(config)
	if err != nil {
		return nil, err
	}

	taskFilter := &ec2.DescribeImportSnapshotTasksInput{
		ImportTaskIds: []*string{importTaskID},
	}

	_, err = compute.DescribeImportSnapshotTasks(taskFilter)
	if err != nil {
		return nil, err
	}

	fmt.Println("waiting for snapshot - can take like 5min.... ")

	waitStartTime := time.Now()

	ct := aws.BackgroundContext()
	w := request.Waiter{
		Name:        "DescribeImportSnapshotTasks",
		Delay:       request.ConstantWaiterDelay(15 * time.Second),
		MaxAttempts: 60,
		Acceptors: []request.WaiterAcceptor{
			{
				State:    request.SuccessWaiterState,
				Matcher:  request.PathAllWaiterMatch,
				Argument: "ImportSnapshotTasks[].SnapshotTaskDetail.Status",
				Expected: "completed",
			},
			{
				State:    request.FailureWaiterState,
				Matcher:  request.PathAnyWaiterMatch,
				Argument: "ImportSnapshotTasks[].SnapshotTaskDetail.Status",
				Expected: "deleted",
			},
			{
				State:    request.FailureWaiterState,
				Matcher:  request.PathAnyWaiterMatch,
				Argument: "ImportSnapshotTasks[].SnapshotTaskDetail.Status",
				Expected: "deleting",
			},
		},
		NewRequest: func(opts []request.Option) (*request.Request, error) {
			req, _ := compute.DescribeImportSnapshotTasksRequest(taskFilter)
			req.SetContext(ct)
			req.ApplyOptions(opts...)
			return req, nil
		},
	}
	w.WaitWithContext(ct)

	fmt.Printf("import done - took %f minutes\n", time.Since(waitStartTime).Minutes())

	describeOutput, err := compute.DescribeImportSnapshotTasks(taskFilter)
	if err != nil {
		return nil, err
	}

	snapshotID := describeOutput.ImportSnapshotTasks[0].SnapshotTaskDetail.SnapshotId

	return snapshotID, nil
}

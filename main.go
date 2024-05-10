package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type AppConfig struct {
	Cluster           string `json:"cluster"`
	TaskDefinitionArn string `json:"taskDefinitionArn"`
	SubnetID          string `json:"subnetId"`
	Region            string `json:"region"`
}

//go:embed config.json
var configJson []byte

// runECSTask runs ECS task
func runECSTask() error {
	var appConfig AppConfig
	if jerr := json.Unmarshal(configJson, &appConfig); jerr != nil {
		return jerr
	}

	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(appConfig.Region))
	if err != nil {
		return err
	}

	client := ecs.NewFromConfig(cfg)
	runTaskIn := &ecs.RunTaskInput{
		TaskDefinition: aws.String(appConfig.TaskDefinitionArn),
		Cluster:        aws.String(appConfig.Cluster),
		NetworkConfiguration: &types.NetworkConfiguration{
			AwsvpcConfiguration: &types.AwsVpcConfiguration{
				Subnets: []string{
					appConfig.SubnetID,
				},
				AssignPublicIp: types.AssignPublicIpEnabled,
			},
		},
		LaunchType: types.LaunchTypeFargate,
	}

	runOut, rerr := client.RunTask(context.TODO(), runTaskIn)
	if rerr != nil {
		return rerr
	}

	taskArn := aws.ToString(runOut.Tasks[0].TaskArn)
	fmt.Println("Task Created.")
	fmt.Println("\tTaskARN: ", taskArn)
	fmt.Printf("\taws ecs describe-tasks --cluster %s --tasks %s\n\n", appConfig.Cluster, taskArn)

	waiter := ecs.NewTasksStoppedWaiter(client)
	waitParams := &ecs.DescribeTasksInput{
		Tasks:   []string{taskArn},
		Cluster: aws.String(appConfig.Cluster),
	}
	maxWaitTime := 5 * time.Minute

	if werr := waiter.Wait(context.TODO(), waitParams, maxWaitTime); werr != nil {
		return werr
	}

	describeIn := &ecs.DescribeTasksInput{
		Tasks:   []string{taskArn},
		Cluster: aws.String(appConfig.Cluster),
	}
	stopOut, serr := client.DescribeTasks(context.TODO(), describeIn)
	if serr != nil {
		return serr
	}

	createdAt := *stopOut.Tasks[0].CreatedAt
	startedAt := *stopOut.Tasks[0].StartedAt
	stoppedAt := *stopOut.Tasks[0].StoppedAt
	takenTime := startedAt.Sub(createdAt)

	fmt.Println("Task Stopped.")
	fmt.Println("\tTaskARN: ", aws.ToString(stopOut.Tasks[0].TaskArn))
	fmt.Println("\tCreatedAt: ", createdAt)
	fmt.Println("\tStartedAt: ", startedAt)
	fmt.Println("\tStoppedAt: ", stoppedAt)
	fmt.Printf("\tTakenTimeToStart: %s", takenTime)

	return nil
}

func main() {
	if err := runECSTask(); err != nil {
		fmt.Println(err.Error())
	}
}

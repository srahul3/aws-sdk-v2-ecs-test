package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	typecw "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/cenkalti/backoff/v4"
)

var subnets = []string{
	"subnet-082ad242218f081d0", "subnet-043bd65c3571ef63b", "subnet-0bbf760006fd628d9",
}

type ECSDriver struct {
	Cluster           string `json:"cluster"`
	TaskDefinitionArn string `json:"taskDefinitionArn"`
	Region            string `json:"region"`
	CWGroupArn        string `json:"cwGroupArn"`
	CWGroupName       string `json:"cwGroupName"`
	client            *ecs.Client
	cwClient          *cloudwatchlogs.Client
	runTaskIn         *ecs.RunTaskInput
	taskID            string
}

func (a *ECSDriver) init() error {
	// appConfig := ECSDriver{
	// 	Cluster:           "tfe-agent-ecs-cluster",
	// 	TaskDefinitionArn: "arn:aws:ecs:us-west-2:980777455695:task-definition/tfe-agent:7",
	// 	Region:            "us-west-2",
	// }

	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(a.Region))
	if err != nil {
		return err
	}

	a.client = ecs.NewFromConfig(cfg)
	// _, err = client.DescribeClusters(context.TODO(), &ecs.DescribeClustersInput{
	// 	Clusters: []string{a.Cluster},
	// })
	// if err != nil {
	// 	return err
	// }

	// _, err = client.DescribeTaskDefinition(context.TODO(), &ecs.DescribeTaskDefinitionInput{
	// 	TaskDefinition: aws.String(a.TaskDefinitionArn),
	// })
	// if err != nil {
	// 	return err
	// }
	a.runTaskIn = &ecs.RunTaskInput{
		TaskDefinition: aws.String(a.TaskDefinitionArn),
		Cluster:        aws.String(a.Cluster),
		NetworkConfiguration: &types.NetworkConfiguration{
			AwsvpcConfiguration: &types.AwsVpcConfiguration{
				Subnets: subnets,
				// needs to be enabled for Fargate
				AssignPublicIp: types.AssignPublicIpEnabled,
			},
		},
		Count:      aws.Int32(1),
		LaunchType: types.LaunchTypeFargate,
	}

	a.cwClient = cloudwatchlogs.NewFromConfig(cfg)

	return nil
}

func (a *ECSDriver) runTask() error {
	// run the task
	runOut, rerr := a.client.RunTask(context.TODO(), a.runTaskIn)
	if rerr != nil {
		return rerr
	}

	taskArn := aws.ToString(runOut.Tasks[0].TaskArn)
	fmt.Println("Task Created.")
	fmt.Println("\tTaskARN: ", taskArn)

	// split the taskArn to get the log stream name
	a.taskID = "tfe-agent/tfe-agent/" + strings.Split(taskArn, "/")[len(strings.Split(taskArn, "/"))-1]
	fmt.Println("\tCloudWatchGroup: ", a.CWGroupName)
	fmt.Println("\tCloudWatchStream: ", a.taskID)

	// start the log streaming
	b := backoff.NewConstantBackOff(1 * time.Second)
	err := backoff.Retry(func() error {
		request := &cloudwatchlogs.StartLiveTailInput{
			LogGroupIdentifiers: []string{a.CWGroupArn},
			LogStreamNames:      []string{a.taskID},
		}

		response, err := a.cwClient.StartLiveTail(context.TODO(), request)
		// Handle pre-stream Exceptions
		if err != nil {
			log.Printf("Failed to start streaming: %v", err)
			return err
		}

		stream := response.GetStream()
		go handleEventStreamAsync(stream)
		return nil
	}, b)

	if err != nil {
		return err
	}

	// wait for the task to stop
	waiter := ecs.NewTasksStoppedWaiter(a.client)
	waitParams := &ecs.DescribeTasksInput{
		Tasks:   []string{taskArn},
		Cluster: aws.String(a.Cluster),
	}
	maxWaitTime := 5 * time.Minute

	stdOut, err := waiter.WaitForOutput(context.TODO(), waitParams, maxWaitTime)
	if err != nil {
		return err
	}

	// marshal the output to pretty print
	out, err := json.MarshalIndent(stdOut, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(out))

	fmt.Println("Task Stopped.")
	fmt.Println("\tTaskARN: ", aws.ToString(stdOut.Tasks[0].TaskArn))
	fmt.Println("\tCreatedAt: ", *stdOut.Tasks[0].CreatedAt)
	fmt.Println("\tStartedAt: ", *stdOut.Tasks[0].StartedAt)
	fmt.Println("\tStoppedAt: ", *stdOut.Tasks[0].StoppedAt)
	fmt.Printf("\tTakenTimeToStart: %s", stdOut.Tasks[0].StartedAt.Sub(*stdOut.Tasks[0].CreatedAt))

	return nil
}

func (a *ECSDriver) getLogs(taskArn string) error {
	// printing the logs
	logsIn := &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(a.CWGroupName),
		LogStreamName: aws.String(a.taskID),
	}
	// cwClient := cloudwatchlogs.NewFromConfig(cfg)
	logsOut, lerr := a.cwClient.GetLogEvents(context.TODO(), logsIn)
	if lerr != nil {
		return lerr
	}

	fmt.Println("")
	fmt.Println("CW Logs:--------------------------------------------------")
	// print the logs to the console
	for e := range logsOut.Events {
		fmt.Println(*logsOut.Events[e].Message)
	}

	return nil
}

func main() {
	// read the environment variables from file setenv.sh
	// reading file content
	file, err := os.ReadFile("setenv.sh")
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	fileContent := strings.Split(string(file), "\n")
	// iterate over each line
	for _, line := range fileContent {
		// if line strats with export
		if !strings.HasPrefix(line, "export") {
			continue
		}
		// remove export from the line
		line = strings.Replace(line, "export ", "", 1)
		// split the line by first "="
		lineSplit := strings.SplitN(line, "=", 2)
		// set the environment variable
		if len(lineSplit) == 2 {
			os.Setenv(lineSplit[0], lineSplit[1])
		}
	}

	driver := ECSDriver{
		Cluster:           "tfe-agent-ecs-cluster",
		TaskDefinitionArn: "arn:aws:ecs:us-west-2:980777455695:task-definition/tfe-agent:7",
		Region:            "us-west-2",
		CWGroupArn:        "arn:aws:logs:us-west-2:980777455695:log-group:demo-cw",
		CWGroupName:       "demo-cw",
	}

	if err := driver.init(); err != nil {
		fmt.Println(err.Error())
	}

	if err := driver.runTask(); err != nil {
		fmt.Println(err.Error())
	}
}

func handleEventStreamAsync(stream *cloudwatchlogs.StartLiveTailEventStream) {
	eventsChan := stream.Events()
	for {
		event := <-eventsChan
		switch e := event.(type) {
		case *typecw.StartLiveTailResponseStreamMemberSessionStart:
			log.Println("Received SessionStart event")
		case *typecw.StartLiveTailResponseStreamMemberSessionUpdate:
			for _, logEvent := range e.Value.SessionResults {
				log.Println(*logEvent.Message)
			}
		default:
			// Handle on-stream exceptions
			if err := stream.Err(); err != nil {
				log.Fatalf("Error occured during streaming: %v", err)
			} else if event == nil {
				log.Println("Stream is Closed")
				return
			} else {
				log.Fatalf("Unknown event type: %T", e)
			}
		}
	}
}

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

type AppConfig struct {
	Cluster           string `json:"cluster"`
	TaskDefinitionArn string `json:"taskDefinitionArn"`
	Region            string `json:"region"`
}

// runECSTask runs ECS task
func runECSTask() error {
	appConfig := AppConfig{
		Cluster:           "tfe-agent-ecs-cluster",
		TaskDefinitionArn: "arn:aws:ecs:us-west-2:980777455695:task-definition/tfe-agent:7",
		Region:            "us-west-2",
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
				Subnets: subnets,
				// needs to be enabled for Fargate
				AssignPublicIp: types.AssignPublicIpEnabled,
			},
		},
		Count:      aws.Int32(1),
		LaunchType: types.LaunchTypeFargate,
	}

	// o, e := client.ListClusters(context.TODO(), &ecs.ListClustersInput{})
	// if e != nil {
	// 	return e
	// }
	// fmt.Println("Clusters:" + o.ClusterArns[0])

	// run the task
	runOut, rerr := client.RunTask(context.TODO(), runTaskIn)
	if rerr != nil {
		return rerr
	}

	taskArn := aws.ToString(runOut.Tasks[0].TaskArn)
	fmt.Println("Task Created.")
	fmt.Println("\tTaskARN: ", taskArn)
	fmt.Printf("\taws ecs describe-tasks --cluster %s --tasks %s\n\n", appConfig.Cluster, taskArn)

	// split the taskArn to get the log stream name
	taskArnPartsLast := "tfe-agent/tfe-agent/" + strings.Split(taskArn, "/")[len(strings.Split(taskArn, "/"))-1]
	fmt.Println("Log Stream Name: ", taskArnPartsLast)

	cwClient := cloudwatchlogs.NewFromConfig(cfg)
	b := backoff.NewConstantBackOff(1 * time.Second)
	backoff.Retry(func() error {
		request := &cloudwatchlogs.StartLiveTailInput{
			LogGroupIdentifiers: []string{"arn:aws:logs:us-west-2:980777455695:log-group:demo-cw"},
			LogStreamNames:      []string{taskArnPartsLast},
		}

		response, err := cwClient.StartLiveTail(context.TODO(), request)
		// Handle pre-stream Exceptions
		if err != nil {
			log.Printf("Failed to start streaming: %v", err)
			return err
		}

		stream := response.GetStream()
		go handleEventStreamAsync(stream)
		return nil
	}, b)

	// wait for the task to stop
	waiter := ecs.NewTasksStoppedWaiter(client)
	waitParams := &ecs.DescribeTasksInput{
		Tasks:   []string{taskArn},
		Cluster: aws.String(appConfig.Cluster),
	}
	maxWaitTime := 5 * time.Minute

	stdOut, err := waiter.WaitForOutput(context.TODO(), waitParams, maxWaitTime)
	if err != nil {
		return err
	}

	// marshal the output to pretty print
	out, err := json.MarshalIndent(stdOut, "", "  ")
	// out, err := json.Marshal(stdOut)
	if err != nil {
		panic(err)
	}

	fmt.Println(string(out))

	// if werr := waiter.Wait(context.TODO(), waitParams, maxWaitTime); werr != nil {
	// 	return werr
	// }

	describeIn := &ecs.DescribeTasksInput{
		Tasks:   []string{taskArn},
		Cluster: aws.String(appConfig.Cluster),
	}
	stopOut, serr := client.DescribeTasks(context.TODO(), describeIn)
	if serr != nil {
		return serr
	}

	if stopOut.Tasks[0].StoppedReason != nil {
		fmt.Println("Task Stopped with reason: ", *stopOut.Tasks[0].StoppedReason)
	}

	createdAt := *stopOut.Tasks[0].CreatedAt
	stoppedAt := *stopOut.Tasks[0].StoppedAt

	fmt.Println("Task Stopped.")
	fmt.Println("\tTaskARN: ", aws.ToString(stopOut.Tasks[0].TaskArn))
	fmt.Println("\tCreatedAt: ", createdAt)

	fmt.Println("\tStoppedAt: ", stoppedAt)

	startedAt := *stopOut.Tasks[0].StartedAt
	takenTime := startedAt.Sub(createdAt)
	fmt.Println("\tStartedAt: ", startedAt)
	fmt.Printf("\tTakenTimeToStart: %s", takenTime)

	// printing the logs
	logsIn := &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String("demo-cw"),
		LogStreamName: aws.String(taskArnPartsLast),
	}
	// cwClient := cloudwatchlogs.NewFromConfig(cfg)
	logsOut, lerr := cwClient.GetLogEvents(context.TODO(), logsIn)
	if lerr != nil {
		return lerr
	}

	fmt.Println("")
	fmt.Println("CW Logs:--------------------------------------------------")
	// print the logs to the console
	for e := range logsOut.Events {
		fmt.Println(*logsOut.Events[e].Message)
	}
	// logsOutStr, _ := json.Marshal(logsOut)
	// fmt.Println(string(logsOutStr))

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

	if err := runECSTask(); err != nil {
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

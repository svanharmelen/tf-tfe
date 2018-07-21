package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	tfe "github.com/hashicorp/go-tfe"
)

const (
	// Fields that are expected in each record.
	bucketField = iota
	keyField
	workspaceField

	// The number of concurrent workers.
	workers = 10
)

// Migrator implements the migration methods.
type Migrator struct {
	client       *tfe.Client
	downloader   *s3manager.Downloader
	organization string
}

// Task represents a single migration task.
type Task struct {
	bucket    string
	key       string
	workspace string

	state *aws.WriteAtBuffer
	meta  *Meta
}

// Meta represents the metadata of a state.
type Meta struct {
	Lineage          string `json:"lineage"`
	Serial           int64  `json:"serial"`
	TerraformVersion string `json:"terraform-version"`
}

func main() {
	input := flag.String("input", "", "The path to a CSV file containing the required input")
	organization := flag.String("organization", "", "The organization that will contain the new workspaces")
	flag.Parse()

	// Check the required inputs
	if input == nil || *input == "" || organization == nil || *organization == "" {
		flag.Usage()
		os.Exit(1)
	}

	// Open the input file to make sure it exists and is readable.
	f, err := os.Open(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening input file: %v\n", err)
		os.Exit(1)
	}

	// Create a new AWS S3 downloader. To configure the client export
	// the usual AWS environment variables:
	//
	// export AWS_ACCESS_KEY_ID=AKID
	// export AWS_SECRET_ACCESS_KEY=SECRET
	// export AWS_REGION=us-east-1
	sess, err := session.NewSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating the AWS client: %v\n", err)
		os.Exit(1)
	}
	downloader := s3manager.NewDownloader(sess)

	// Create a new TFE client. To configure a custom (PTFE) endpoint
	// and your token, export the following environment variables:
	//
	// export TFE_ADDRESS=https://ptfe.company.com
	// export TFE_TOKEN=your-personal-token
	//
	// TFE_ADDRESS defaults to https://app.terraform.io if not provided.
	client, err := tfe.NewClient(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating the TFE client: %v\n", err)
		os.Exit(1)
	}

	m := &Migrator{
		client:       client,
		downloader:   downloader,
		organization: *organization,
	}

	// Create a new CSV reader to read the input file.
	r := csv.NewReader(f)

	// Read true the input file and create a task for each record. We don't
	// want to exit while we are already start migrating states, so we first
	// read all records and create all tasks, before executing the tasks.
	var tasks []*Task
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading	CSV file: %v\n", err)
			os.Exit(1)
		}
		if len(record) != 3 {
			fmt.Fprintf(os.Stderr, "Unexpected number of fields in record: %v", record)
			os.Exit(1)
		}
		tasks = append(tasks, &Task{
			bucket:    record[bucketField],
			key:       record[keyField],
			workspace: record[workspaceField],
			state:     aws.NewWriteAtBuffer(nil),
			meta:      &Meta{},
		})
	}

	// Create a new waitgroup and a buffered queue channel so
	// we can migrate multiple states concurrently.
	var wg sync.WaitGroup
	queue := make(chan *Task, 100)

	// Start the workers.
	for i := 0; i < workers; i++ {
		go m.worker(&wg, queue)
	}

	// And now start the queueing the buffered tasks
	for _, task := range tasks {
		wg.Add(1)
		queue <- task
	}

	wg.Wait()
	fmt.Printf("\nFinished migrating states.\n")
}

func (m *Migrator) worker(wg *sync.WaitGroup, queue <-chan *Task) {
	for task := range queue {
		err := func(task *Task) error {
			err := m.downloadState(task)
			if err != nil {
				return err
			}

			w, err := m.createWorkspace(task)
			if err != nil {
				return err
			}

			return m.uploadState(task, w)
		}(task)
		if err != nil {
			log.Printf("Error migrating state for worspace %q: %v", task.workspace, err)
		} else {
			log.Printf("Succesfully migrated state for worspace %q", task.workspace)
		}

		wg.Done()
	}
}

// downloadState downloads and returns the state from S3.
func (m *Migrator) downloadState(t *Task) error {
	_, err := m.downloader.Download(t.state,
		&s3.GetObjectInput{
			Bucket: aws.String(t.bucket),
			Key:    aws.String(t.key),
		},
	)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(t.state.Bytes(), t.meta); err != nil {
		return nil
	}

	if t.meta.Lineage == "" || t.meta.TerraformVersion == "" {
		return fmt.Errorf("Unable to retrieve required fields from the state file: %v", t.meta)
	}

	return nil
}

// createWorkspace creates a new workspqce.
func (m *Migrator) createWorkspace(t *Task) (*tfe.Workspace, error) {
	options := tfe.WorkspaceCreateOptions{
		Name:             tfe.String(t.workspace),
		TerraformVersion: tfe.String(t.meta.TerraformVersion),
	}

	// Create the new workspace.
	return m.client.Workspaces.Create(context.Background(), m.organization, options)
}

// uploadState uploads the state to the new workspace.
func (m *Migrator) uploadState(t *Task, w *tfe.Workspace) error {
	options := tfe.StateVersionCreateOptions{
		Lineage: tfe.String(t.meta.Lineage),
		Serial:  tfe.Int64(t.meta.Serial),
		MD5:     tfe.String(fmt.Sprintf("%x", md5.Sum(t.state.Bytes()))),
		State:   tfe.String(base64.StdEncoding.EncodeToString(t.state.Bytes())),
	}

	// Create the new state..
	_, err := m.client.StateVersions.Create(context.Background(), w.ID, options)
	return err
}

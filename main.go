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
	"net/url"
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
	projectField
	repoField
	branchField
	configFileField
	workspaceField

	// The number of concurrent workers.
	workers = 10
)

// Migrator implements the migration methods.
type Migrator struct {
	client       *tfe.Client
	downloader   *s3manager.Downloader
	hostname     string
	organization string
}

// Task represents a single migration task.
type Task struct {
	bucket     string
	key        string
	project    string
	repo       string
	branch     string
	configFile string
	workspace  string

	state *aws.WriteAtBuffer
	meta  *Meta
}

// Meta represents the metadata of a state.
type Meta struct {
	Lineage          string `json:"lineage"`
	Serial           int64  `json:"serial"`
	TerraformVersion string `json:"terraform_version"`
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

	// Set the Bitbucket address and personal access token. To set a
	// custom address and to provide a token, export the following
	// variables:
	//
	// export BITBUCKET_ADDRESS=https://bitbucket.company.com
	// export BITBUCKET_TOKEN=MDM0MjM5NDc2MDxxxxxxxxxxxxxxxxxxxxx
	//
	// BITBUCKET_ADDRESS defaults to https://bitbucket.org if not provided.
	bitbucketAddess = os.Getenv("BITBUCKET_ADDRESS")
	if bitbucketAddess == "" {
		bitbucketAddess = "https://bitbucket.org"
	}
	bitbucketToken = os.Getenv("BITBUCKET_TOKEN")
	if bitbucketToken == "" {
		fmt.Fprintln(os.Stderr, "Required Bitbucket token not found")
		os.Exit(1)
	}

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
		hostname:     "app.terraform.io",
		organization: *organization,
	}

	// We need the TFE hostname for in the backend configuration block. So
	// see if one is provided we use that instead of the default hostname.
	if tfeAddress := os.Getenv("TFE_ADDRESS"); tfeAddress != "" {
		u, err := url.Parse(tfeAddress)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing the TFE address: %v\n", err)
			os.Exit(1)
		}
		m.hostname = u.Hostname()
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
		if len(record) != 7 {
			fmt.Fprintf(
				os.Stderr,
				"Unexpected number of fields (%d) in record: %v\n", len(record), record,
			)
			os.Exit(1)
		}
		tasks = append(tasks, &Task{
			bucket:     record[bucketField],
			key:        record[keyField],
			project:    record[projectField],
			repo:       record[repoField],
			branch:     record[branchField],
			configFile: record[configFileField],
			workspace:  record[workspaceField],
			state:      aws.NewWriteAtBuffer(nil),
			meta:       &Meta{},
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

			err = m.uploadState(task, w)
			if err != nil {
				return err
			}

			return m.updateBackend(task)
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

func (m *Migrator) updateBackend(t *Task) error {
	content, err := readBitbucketFile(t)
	if err != nil {
		return fmt.Errorf("Failed to read config file %q from Bitbucket: %v", t.configFile, err)
	}

	start, end := findTerraformBlock(content)
	if start == -1 || end == -1 {
		return fmt.Errorf("No terraform configuration block found in %q", t.configFile)
	}

	tfBlock := fmt.Sprintf(backendConfig, m.hostname, m.organization, t.workspace)
	content = content[0:start] + tfBlock + content[end:]

	if err := writeBitbucketFile(t, content); err != nil {
		return fmt.Errorf("Failed to write config file %q from Bitbucket: %v", t.configFile, err)
	}

	return nil
}

func findTerraformBlock(content string) (start, end int) {
	startPos := -1
	openBr := 0

	for pos, r := range content {
		if startPos == -1 {
			if pos+9 < len(content) && content[pos:pos+9] != "terraform" {
				continue
			}
			startPos = pos
		}
		switch r {
		case '{':
			openBr++
		case '}':
			openBr--
		default:
			continue
		}

		if openBr == 0 && startPos != -1 {
			return startPos, pos + 1
		}
	}

	return -1, -1
}

const backendConfig = `terraform {
  backend "remote" {
    hostname     = "%s"
    organization = "%s"

    workspaces {
      name = "%s"
    }
  }
}`

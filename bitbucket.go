package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
)

const (
	commitURL = "%s/rest/api/latest/projects/%s/repos/%s/commits?limit=1"
	repoURL   = "%s/rest/api/latest/projects/%s/repos/%s/browse/%s?at=%s"
)

var (
	bitbucketAddess string
	bitbucketToken  string
)

func getLatestCommitID(t *Task) (string, error) {
	// Compose the URL for the given task..
	u := fmt.Sprintf(commitURL, bitbucketAddess, t.project, t.repo)

	// Create the request.
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bitbucketToken)

	// Make the API call to receive the latest commit.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Check the response for any errors.
	if err = checkResponse(resp); err != nil {
		return "", err
	}

	var commits struct {
		Values []struct {
			CommitID string `json:"id"`
		} `json:"values"`
	}

	// Parse the response to retrieve the commit ID.
	err = json.NewDecoder(resp.Body).Decode(&commits)
	if err != nil {
		return "", err
	}

	if len(commits.Values) != 1 {
		return "", fmt.Errorf("could not find latest commit")
	}

	return commits.Values[0].CommitID, nil
}

func readBitbucketFile(t *Task) (string, error) {
	// Compose the URL for the given task..
	u := fmt.Sprintf(repoURL, bitbucketAddess, t.project, t.repo, t.configFile, t.branch)

	// Create the request.
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bitbucketToken)

	// Make the API call to read the file.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Check the response for any errors.
	if err = checkResponse(resp); err != nil {
		return "", err
	}

	var content struct {
		Lines []struct {
			Text string `json:"text"`
		} `json:"lines"`
	}

	// Parse the response to retrieve the file content.
	err = json.NewDecoder(resp.Body).Decode(&content)
	if err != nil {
		return "", err
	}

	buf := bytes.NewBuffer(nil)
	for _, line := range content.Lines {
		buf.WriteString(line.Text + "\n")
	}

	return buf.String(), nil
}

func writeBitbucketFile(t *Task, content string) error {
	// First get the current commit.
	commitID, err := getLatestCommitID(t)
	if err != nil {
		return err
	}

	// Compose the URL for the given task..
	u := fmt.Sprintf(repoURL, bitbucketAddess, t.project, t.repo, t.configFile, t.branch)

	buf := new(bytes.Buffer)
	mw := multipart.NewWriter(buf)

	// This (undocumented) API call requires a From MultiPart request body
	// containing the branch, commit ID, message and updated file content.
	fw, err := mw.CreateFormField("branch")
	if err != nil {
		return err
	}

	// Add the branch.
	if _, err = fw.Write([]byte("refs/heads/" + t.branch)); err != nil {
		return err
	}

	if fw, err = mw.CreateFormField("sourceCommitId"); err != nil {
		return err
	}

	// Add the commit ID.
	if _, err = fw.Write([]byte(commitID)); err != nil {
		return err
	}

	if fw, err = mw.CreateFormField("message"); err != nil {
		return err
	}

	// Add a custom message.
	if _, err = fw.Write([]byte("Backend configuration updated by migration tool")); err != nil {
		return err
	}

	if fw, err = mw.CreateFormFile("content", "blob"); err != nil {
		return err
	}

	// Add the updated fiel content.
	if _, err = fw.Write([]byte(content)); err != nil {
		return err
	}

	if err := mw.Close(); err != nil {
		return err
	}

	// Create the request.
	req, err := http.NewRequest("PUT", u, buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bitbucketToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	// Make the API call to write and commit the updated file.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return checkResponse(resp)
}

func checkResponse(resp *http.Response) error {
	if resp.StatusCode == 200 {
		return nil
	}

	var response struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	// If we received an unexpected response code, try to parse the error
	// in order to get a descriptive error. If that fails, we just return
	// the received HTTP status instead.
	err := json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return fmt.Errorf("error decoding response: %v", err)
	}

	if len(response.Errors) == 0 {
		return fmt.Errorf("unexpected response: %s", resp.Status)
	}

	return fmt.Errorf(response.Errors[0].Message)
}

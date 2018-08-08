# tf-tfe
Working example for migrating TF to TFE

_NOTE: This is a working example for bulk migrating from TF OSS to TFE. The
example assumes state is currently stored in S3 and Bitbucket is used as VCS.
Please note that this example does not come with any unit tests and is not
officially supported!_

## Installation and Usage

Installation can be done with a normal `go get`:

```
$ go get github.com/svanharmelen/tf-tfe
```

Once installed you can execute the program:

```
$ tf-tfe -h
Usage of tf-tfe:
  -input string
        The path to a CSV file containing the required input
  -organization string
        The organization that will contain the new workspaces

$ tf-tfe -input=./example.csv -organization=my-org-name
2018/08/08 14:30:54 Succesfully migrated state for worspace "svh-app-default"

Finished migrating states.
```

## Configuration

There is no configuration file for this example, but there are a few mandatory
environment variables that need to be set in order to configure endpoints and
credentials.

#### AWS - S3

To configure the S3 client export the usual AWS environment variables:

```
$ export AWS_ACCESS_KEY_ID=AKID
$ export AWS_SECRET_ACCESS_KEY=SECRET
$ export AWS_REGION=us-east-1
```

#### Bitbucket

To set a custom address and to provide a token, export the following variables:

```
$ export BITBUCKET_ADDRESS=https://bitbucket.company.com
$ export BITBUCKET_TOKEN=MDM0MjM5NDc2MDxxxxxxxxxxxxxxxxxxxxx
```

BITBUCKET_ADDRESS defaults to https://bitbucket.org if not provided.

#### Terraform Enterprise

To configure a custom (PTFE) endpoint and your token, export the following
environment variables:

```
$ export TFE_ADDRESS=https://ptfe.company.com
$ export TFE_TOKEN=your-personal-token
```

TFE_ADDRESS defaults to https://app.terraform.io if not provided.

## Input file format

The input file must be a CSV file that contains the following fields:

  * bucket - S3 bucket name containing the Terraform state file
  * key - Object name of the Terraform state file
  * project - Bitbucket project containing your Terraform repository
  * repo - Bitbucket repository hosting the Terraform configuration files
  * branch - Bitbucket branch to use
  * backend - Terraform configuration file that contains the S3 backend config
  * worspace - Name of the new TFE workspace for this Terraform configuration

Please see `example.csv` in this repo as a very simple example input file.

## Issues and Contributing

If you find an issue with this example, please report an issue. If you'd
like, I welcome any contributions. Fork this repo and submit a pull
request.

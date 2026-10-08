package queue

import (
	"context"
	"errors"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// AWSConfig holds the bits needed to point the SQS client at real
// AWS or the local ElasticMQ. The two callers (cmd/api and cmd/worker)
// construct this from environment variables; centralizing the field
// set here keeps them identical.
type AWSConfig struct {
	Region      string // "us-east-1" in prod, anything local
	EndpointURL string // "" in prod; "http://localhost:9324" locally
	// QueueURL is the SQS queue to publish to / consume from.
	// In prod: https://sqs.us-east-1.amazonaws.com/123456789012/reservations
	// Locally: http://localhost:9324/000000000000/reservations
	QueueURL string
}

// NewSQSClient builds an *sqs.Client from the AWSConfig. When
// EndpointURL is non-empty, dummy credentials are injected because
// ElasticMQ does not authenticate but the SDK still requires them.
//
// This is the shared construction used by both the API publisher
// (Phase 6: SQSPublisher) and the worker (Phase 6: queue.Poll).
func NewSQSClient(ctx context.Context, cfg AWSConfig) (*sqs.Client, error) {
	if cfg.Region == "" {
		return nil, errors.New("queue: AWS region is required")
	}
	if cfg.QueueURL == "" {
		return nil, errors.New("queue: queue URL is required")
	}

	if cfg.EndpointURL != "" {
		// Local dev: inject any non-empty creds so the SDK is happy.
		// Production callers leave the env alone and use the standard
		// credential chain (IAM role, env vars, ~/.aws/credentials).
		if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
			_ = os.Setenv("AWS_ACCESS_KEY_ID", "local")
		}
		if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
			_ = os.Setenv("AWS_SECRET_ACCESS_KEY", "local")
		}
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, err
	}

	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
		}
	})
	return client, nil
}
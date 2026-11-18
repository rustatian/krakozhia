package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors/types"
)

const (
	embedModel = "amazon.titan-embed-text-v2:0"
	dimensions = 1024
	// textKey is the metadata key that holds the note text for the full-text search.
	textKey = "text"
)

func awsClients(ctx context.Context, c config) (*bedrockruntime.Client, *s3vectors.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.Region), awsconfig.WithSharedConfigProfile(c.Profile))
	if err != nil {
		return nil, nil, err
	}
	return bedrockruntime.NewFromConfig(cfg), s3vectors.NewFromConfig(cfg), nil
}

// embed makes the vector of a text with Amazon Titan Text Embeddings V2.
// https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-titan-embed-text.html
func embed(ctx context.Context, br *bedrockruntime.Client, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"inputText": text, "dimensions": dimensions, "normalize": true}) // cannot fail for these types
	out, err := br.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(embedModel),
		ContentType: aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	var resp struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	return resp.Embedding, nil
}

// createIndex makes the vector index. Its dimension, its metric, its non-filterable keys and the analyzer of its text key are fixed at creation.
// https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-vectors-configuring-full-text-search.html
func createIndex(ctx context.Context, sv *s3vectors.Client, c config) error {
	_, err := sv.CreateIndex(ctx, &s3vectors.CreateIndexInput{
		VectorBucketName: &c.Bucket,
		IndexName:        &c.Index,
		DataType:         types.DataTypeFloat32,
		Dimension:        aws.Int32(dimensions),
		DistanceMetric:   types.DistanceMetricCosine,
		MetadataConfiguration: &types.MetadataConfiguration{
			NonFilterableMetadataKeys: []string{textKey},
			TextSearchConfigurations:  []types.TextSearchConfiguration{{MetadataKey: aws.String(textKey), Language: types.LanguageCodeEn}},
		},
	})
	return err
}

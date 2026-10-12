// Com AWS_ENDPOINT_URL preenchido o SDK fala com o LocalStack; vazio, usa o endpoint
// real da AWS. O mesmo caminho de código serve local e produção. As credenciais vêm do
// ambiente (SigV4 exige alguma chave; no LocalStack o valor é irrelevante).
package awsclient

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
)

// NewSQSClient cria o cliente de SQS.
func NewSQSClient(ctx context.Context, region, endpoint string) (*awssqs.Client, error) {
	if region == "" {
		return nil, fmt.Errorf("awsclient: region is required")
	}

	// Resolve credenciais e região pelos provedores do SDK.
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("awsclient: cannot load AWS config: %w", err)
	}

	return awssqs.NewFromConfig(cfg, func(options *awssqs.Options) {
		if endpoint != "" {
			options.BaseEndpoint = &endpoint
		}
	}), nil
}

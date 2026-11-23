# Datacircle tools for Amazon Bedrock in the AWS SDK for Go

Find the best GTM APIs. We benchmark all the GTM APIs and we put the results on our website.

Our website: https://datacircle.dev

This Go module gives Amazon Bedrock's Converse API four tools that call Datacircle's open API, https://api.datacircle.dev/v1. You pass the tools to `Converse` or `ConverseStream` of the [AWS SDK for Go v2](https://github.com/aws/aws-sdk-go-v2). Bedrock does not run tools. This module runs each call the model makes to a Datacircle tool. You get the answers to send back to the model. You need no key and no account.

| Tool | What the tool gives | Call |
| --- | --- | --- |
| `datacircle_list_apis` | Every GTM API on Datacircle, with the API's slug, number of plans, number of providers and best plan | `GET /apis` |
| `datacircle_best_plans` | One API's plans, sorted by score out of 100, best first. Each plan comes with a rank and a price per 1,000 calls | `GET /apis/{slug}/best` |
| `datacircle_search_plans` | Plans that match a provider or plan name, on all APIs or on one API. You can sort them by score, provider or price | `GET /plans` |
| `datacircle_provider` | The APIs a provider sells, and the provider's rank on each API | `GET /providers/{slug}` |

## Install

```bash
go get github.com/waynehamadi/bedrock-go-datacircle
```

The module needs Go 1.22 or later. The AWS SDK's `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` must be v1.25.0 or later.

## Use the tools with Converse

Put `tools.Tools()` in `ToolConfig`. Add the model's message to your messages. Then add the user message from `tools.Results`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	datacircle "github.com/waynehamadi/bedrock-go-datacircle"
)

func main() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		log.Fatal(err)
	}
	client := bedrockruntime.NewFromConfig(cfg)
	tools := datacircle.New(nil)

	question := &types.ContentBlockMemberText{Value: "Who are the 3 best providers for the email-finder API?"}
	input := &bedrockruntime.ConverseInput{
		ModelId:    aws.String("amazon.nova-pro-v1:0"),
		Messages:   []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{question}}},
		ToolConfig: &types.ToolConfiguration{Tools: tools.Tools()},
	}
	for {
		out, err := client.Converse(ctx, input)
		if err != nil {
			log.Fatal(err)
		}
		message := out.Output.(*types.ConverseOutputMemberMessage).Value
		input.Messages = append(input.Messages, message)
		answers, ok := tools.Results(ctx, message, nil)
		if !ok {
			for _, block := range message.Content {
				if text, ok := block.(*types.ContentBlockMemberText); ok {
					fmt.Println(text.Value)
				}
			}
			break
		}
		input.Messages = append(input.Messages, answers)
	}
}
```

`tools.Results` gives one user message with a `toolResult` for each tool the model used, in order. `ok` is `false` when the model used no tool.

## Use the tools with ConverseStream

Give each event of the stream to an `Accumulator`. `Add` gives the text of the event. Print that text to show the answer during the stream. After the stream ends, `Message` gives the model's message:

```go
input := &bedrockruntime.ConverseStreamInput{
	ModelId:    aws.String("amazon.nova-pro-v1:0"),
	Messages:   []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{question}}},
	ToolConfig: &types.ToolConfiguration{Tools: tools.Tools()},
}
for {
	out, err := client.ConverseStream(ctx, input)
	if err != nil {
		log.Fatal(err)
	}
	var events datacircle.Accumulator
	stream := out.GetStream()
	for event := range stream.Events() {
		fmt.Print(events.Add(event))
	}
	stream.Close()
	if err := stream.Err(); err != nil {
		log.Fatal(err)
	}
	message := events.Message()
	input.Messages = append(input.Messages, message)
	answers, ok := tools.Results(ctx, message, nil)
	if !ok {
		break
	}
	input.Messages = append(input.Messages, answers)
}
```

The message holds the text, the reasoning and each tool use. The reasoning keeps the model's signature. Bedrock needs that signature back when the model reasons before a tool call. The stream sends each tool input in pieces of JSON. The message holds the whole input. An empty input is `{}`.

`datacircle.Collect(out.GetStream())` reads the stream to the end. Then `Collect` gives the message and the stream's error. Use `Collect` when your code does not print the answer during the stream.

## Your own tools

Bedrock needs an answer to each tool the model used in a turn. If you add tools of your own, pass a function as the last argument of `tools.Results`. The function gets each use of one of your tools and returns the answer as text. If the function returns an error, the model reads `{"error": "..."}`:

```go
input.ToolConfig = &types.ToolConfiguration{Tools: append(tools.Tools(), yourTool)}

answers, ok := tools.Results(ctx, message, func(ctx context.Context, use types.ToolUseBlock) (string, error) {
	var in struct{ Zone string }
	if err := use.Input.UnmarshalSmithyDocument(&in); err != nil {
		return "", err
	}
	return `{"time": "12:00"}`, nil
})
```

Pass `nil` instead of a function, and the model reads an error for each use of one of your tools. `tools.Handles(name)` tells you if the tool named `name` is one of ours. `tools.Result(ctx, use)` answers one use of a Datacircle tool. `datacircle.ToolUses(message)` lists the tool uses in a message for your code to answer. The list leaves out system tools. Bedrock answers system tools itself.

## Make each call yourself

You can make each call without a model. Give `Run` the tool's name and the input as JSON. You get the tool's answer as JSON:

```go
answer, err := datacircle.New(nil).Run(ctx, datacircle.BestPlansName, `{"slug":"email-finder","limit":3}`)
```

## What the model reads

Each tool gives the model a JSON object as the text of the `toolResult`. Each plan comes with a rank, provider, plan, score, price per 1,000 (`perThousand`, in dollars) and a page on datacircle.dev. That page lists every field. Each tool also takes an API's name or a provider's name in place of a slug.

For an unknown slug, the model reads `{"error": "..."}` with the API's message and what to send instead. If the network fails, the error gives the network's message. The model can read the error, call the tool again and keep going. `Run` gives the API's message or the network's message as the error.

Set `HTTPClient` in `datacircle.Config` to give the tools a client with a proxy or a timeout. Pass `nil` to `datacircle.New` and the tools use `http.DefaultClient`.

## Network

Each tool call sends HTTPS GET requests to https://api.datacircle.dev/v1 and nowhere else. Most calls send one request. `datacircle_search_plans` sends up to three when the first search finds nothing. The requests hold only the tool's input from the model: no key and nothing from your messages. The API takes up to 60 calls a minute from one IP address. The tool waits once, for a minute at most, when the API says to wait. The module depends only on the AWS SDK for Go v2, `smithy-go` and `golang.org/x/text`. The module stores nothing.

## Links

- Datacircle: https://datacircle.dev
- The API: https://datacircle.dev/api
- The API's spec: https://api.datacircle.dev/v1/openapi.json
- What each key means: https://datacircle.dev/fields

MIT license.

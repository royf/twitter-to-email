package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/dghubble/go-twitter/twitter"
	"github.com/dghubble/oauth1"
	"github.com/peterbourgon/ff"
)

var (
	// Configuration
	bucket,
	consumer_api_key,
	consumer_api_secret_key,
	access_token,
	access_token_secret,
	email *string

	sess = session.Must(session.NewSession())
)

// formatDate formats dates into a valid S3 key
func formatDate(date time.Time) string {
	return fmt.Sprintf("tweets/%d-%02d-%02d-%d/tweets.json", date.Year(), date.Month(), date.Day(), date.Hour() / 8)
}

// getTodaysKey returns a valid key name derived from the current date in UTC
func getTodaysKey() string {
	return formatDate(time.Now().UTC())
}

// getYesterdaysKey returns a valid key name derived from the previous day in UTC
func getYesterdaysKey() string {
	return formatDate(time.Now().UTC().Add(time.Hour * (-8)))
}

// getStoredTweets retrieves stored tweets from a given key in the S3 bucket
func getStoredTweets(key string) ([]twitter.Tweet, error) {
	svc := s3.New(sess)
	fmt.Printf("Getting tweets from: s3://%s/%s\n", *bucket, key)
	result, err := svc.GetObject(&s3.GetObjectInput{
		Bucket: bucket,
		Key:    aws.String(key),
	})

	if err != nil {
		return nil, err
	}

	var tweets []twitter.Tweet
	err = json.NewDecoder(result.Body).Decode(&tweets)
	return tweets, err
}

// uploadTweets uploads tweets into S3 bucket at given key
func uploadTweets(key string, tweets []twitter.Tweet) error {
	uploader := s3manager.NewUploader(sess)
	buf := bytes.NewBuffer([]byte{})
	err := json.NewEncoder(buf).Encode(tweets)
	if err != nil {
		return err
	}

	fmt.Printf("Uploading %d tweets to s3://%s/%s\n", len(tweets), *bucket, key)
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: bucket,
		Key:    aws.String(key),
		Body:   buf,
	})

	if err != nil {
		return err
	}
	return nil
}

// getNewTweets retrieves tweets newer than sinceID using the Twitter API
func getNewTweets(sinceID int64) ([]twitter.Tweet, error) {
	config := oauth1.NewConfig(*consumer_api_key, *consumer_api_secret_key)
	token := oauth1.NewToken(*access_token, *access_token_secret)
	// OAuth1 http.Client will automatically authorize Requests
	httpClient := config.Client(oauth1.NoContext, token)

	// Twitter client
	client := twitter.NewClient(httpClient)

	// Home Timeline
	homeTimelineParams := &twitter.HomeTimelineParams{
		SinceID:   sinceID,
		TweetMode: "extended",
		Count:     200,
	}
	tweets, _, err := client.Timelines.HomeTimeline(homeTimelineParams)
	if err != nil {
		return nil, err
	}

	fmt.Printf("%d New Tweets Found\n", len(tweets))

	return tweets, nil
}

// TODO document this
func fetchTweets() error {
	today := getTodaysKey()
	storedTweets, err := getStoredTweets(today)

	var sinceID int64
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchKey:
				fmt.Printf("%s not found. Trying to retrieve yesterday’s tweets\n", today)
				yesterday := getYesterdaysKey()
				storedTweets, err := getStoredTweets(yesterday)
				if err != nil {
					if aerr, ok := err.(awserr.Error); ok {
						switch aerr.Code() {
						case s3.ErrCodeNoSuchKey:
							fmt.Printf("%s not found.\n", yesterday)
						default:
							return aerr
						}
					} else {
						return err
					}
				}

				if len(storedTweets) > 0 {
					fmt.Println("Emailing yesterday’s tweets")
					err = emailTweets(storedTweets)
					if err != nil {
						return err
					}

					// Find last tweet from yesterday
					lastTweet := storedTweets[0]
					for _, tweet := range storedTweets {
						if tweet.ID > lastTweet.ID {
							lastTweet = tweet
						}
					}

					sinceID = lastTweet.ID

					storedTweets = []twitter.Tweet{lastTweet}
					fmt.Println("Uploading last tweet from yesterday for tracking")
				} else {
					fmt.Printf("Uploading an empty array to %s\n", today)
				}

				err = uploadTweets(today, storedTweets)
				if err != nil {
					return err
				}
			default:
				return aerr
			}
		} else {
			return aerr
		}
	} else {
		fmt.Printf("%d Older Tweets Found\n", len(storedTweets))

		for _, tweet := range storedTweets {
			if tweet.ID > sinceID {
				sinceID = tweet.ID
			}
		}
	}

    fmt.Printf("Getting new tweets since %d\n", sinceID)
	newTweets, err := getNewTweets(sinceID)

	if err != nil {
		return err
	}

	if len(newTweets) == 0 {
		// Nothing more to do
		return nil
	}

	tweets := append(newTweets, storedTweets...)

	return uploadTweets(today, tweets)
}

// emailTweets formats and emails tweets
func emailTweets(tweets []twitter.Tweet) error {
	builder := strings.Builder{}

	for i := len(tweets) - 1; i > -1; i-- {
		tweet := tweets[i]
		builder.WriteString(buildTweet(&tweet))
	}

	svc := ses.New(session.Must(session.NewSession(&aws.Config{
		Region: aws.String("us-west-2")}, // SES is only available in limited AWS regions, so we hardcode the region here.
	)))

	// Assemble the email.
	input := &ses.SendEmailInput{
		Destination: &ses.Destination{
			CcAddresses: []*string{},
			ToAddresses: []*string{
				email,
			},
		},
		Message: &ses.Message{
			Body: &ses.Body{
				Html: &ses.Content{
					Charset: aws.String("UTF-8"),
					Data:    aws.String(builder.String()),
				},
			},
			Subject: &ses.Content{
				Charset: aws.String("UTF-8"),
				Data:    aws.String("Tweets from the past 8h"),
			},
		},
		Source: email,
	}

	// Attempt to send the email.
	_, err := svc.SendEmail(input)
	return err
}


func buildTweet(tweet *twitter.Tweet) string {
	builder := strings.Builder{}
    builder.WriteString(`
<div style="margin-bottom: 10px; font: 15px system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Ubuntu, 'Helvetica Neue', sans-serif;">
    `)
    if tweet.RetweetedStatus != nil {
        html := `
  <div style="display: flex;">
    <svg viewBox="0 0 24 24" style="color: rgb(45, 51, 55); fill: currentcolor; width: 13px;">
      <g>
        <path d="M23.615 15.477c-.47-.47-1.23-.47-1.697 0l-1.326 1.326V7.4c0-2.178-1.772-3.95-3.95-3.95h-5.2c-.663 0-1.2.538-1.2 1.2s.537 1.2 1.2 1.2h5.2c.854 0 1.55.695 1.55 1.55v9.403l-1.326-1.326c-.47-.47-1.23-.47-1.697 0s-.47 1.23 0 1.697l3.374 3.375c.234.233.542.35.85.35s.613-.116.848-.35l3.375-3.376c.467-.47.467-1.23-.002-1.697zM12.562 18.5h-5.2c-.854 0-1.55-.695-1.55-1.55V7.547l1.326 1.326c.234.235.542.352.848.352s.614-.117.85-.352c.468-.47.468-1.23 0-1.697L5.46 3.8c-.47-.468-1.23-.468-1.697 0L.388 7.177c-.47.47-.47 1.23 0 1.697s1.23.47 1.697 0L3.41 7.547v9.403c0 2.178 1.773 3.95 3.95 3.95h5.2c.664 0 1.2-.538 1.2-1.2s-.535-1.2-1.198-1.2z"></path>
      </g>
    </svg>
    <a href="%s" style="color: rgb(136, 153, 166); font-size: 14px; margin-left: 105px; text-decoration: none;">%s Retweeted</a>
  </div>
        `
        retweeter_url := fmt.Sprintf("https://twitter.com/%s", tweet.User.ScreenName)
        builder.WriteString(fmt.Sprintf(
            html,
            retweeter_url,
            tweet.User.Name,
        ))
        tweet = tweet.RetweetedStatus
    }
    html := `
  <div style="display: flex;">
    <a href="%s" style="border-radius: 9999px; flex-shrink: 0; margin-right: 5px; max-height: 100px; min-width: 100px; overflow: hidden;">
      <img src="%s" style="height: 100px; width: 100px;">
    </a>
    <div>
      <div>
        <a href="%s" style="color: rgb(45, 51, 55); text-decoration: none;">
          <span style="font-weight: bold;">%s</span>
          <span style="color: rgb(136, 153, 166);">@%s</span>
        </a>
      </div>
      <div style="line-height: 1.3125; width: 50%%;">
        <a href="%s" style="color: black; text-decoration: none;">%s</a>
      </div>
    </div>
  </div>
</div>
    `
    tweeter_url := fmt.Sprintf("https://twitter.com/%s", tweet.User.ScreenName)
    tweeter_image := strings.Replace(tweet.User.ProfileImageURLHttps, "_normal.", "_reasonably_small.", 1)
    tweet_url := fmt.Sprintf("https://twitter.com/%s/status/%d", tweet.User.ScreenName, tweet.ID)
    builder.WriteString(fmt.Sprintf(
        html,
        tweeter_url,
        tweeter_image,
        tweeter_url,
        tweet.User.Name,
        tweet.User.ScreenName,
        tweet_url,
        tweet.FullText))

	return builder.String()
}

// getConfig populates the config variables from a JSON file
func getConfig() {
	fs := flag.NewFlagSet("twitter-to-email", flag.ExitOnError)

	bucket = fs.String("bucket", "", "S3 Bucket")
	consumer_api_key = fs.String("consumer-api-key", "", "Twitter Consumer API Key")
	consumer_api_secret_key = fs.String("consumer-api-secret-key", "", "Twitter Consumer API Secret Key")
	access_token = fs.String("access-token", "", "Twitter Access token")
	access_token_secret = fs.String("access-token-secret", "", "Twitter Access token secret")
	email = fs.String("email", "", "Email")

	ff.Parse(fs, []string{},
		ff.WithConfigFile("config.json"),
		ff.WithConfigFileParser(ff.JSONParser))
}

func main() {
	getConfig()
	lambda.Start(fetchTweets)
}

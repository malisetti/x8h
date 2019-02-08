package main

import (
	"sync"
	"time"
)

type appMode string
type config struct {
	Mode                        appMode       `json:"mode"`
	LogChanges                  bool          `json:"logChanges"`
	TweetChanges                bool          `json:"tweetChanges"`
	ReadFromFile                bool          `json:"readFromFile"`
	ReadFromHN                  bool          `json:"readFromHN"`
	CheckFrontPageBeforeRemoval bool          `json:"checkFrontPageBeforeRemoval"`
	Port                        int           `json:"port"`
	TemplateFilePath            string        `json:"templateFilePath"`
	TopStoriesURL               string        `json:"topStoriesUrl"`
	StoryURL                    string        `json:"storyUrl"`
	HNPostLink                  string        `json:"hnPostLink"`
	FrontPageNumArticles        int           `json:"frontPageNumArticles"`
	HNPollTime                  time.Duration `json:"hnPollTime"`
	RateLimit                   string        `json:"rateLimit"`
	DeletionPeriod              time.Duration `json:"deletionPeriod"`
	HumanTrackingLimit          int           `json:"humanTrackingLimit"`
	ItemFromFile                string        `json:"itemFromFile"`
	ItemFromHN                  string        `json:"itemFromHN"`
	TwConsumerAPIKey            string        `json:"twitterConsumerAPIKey"`
	TwConsumerSecretKey         string        `json:"twitterConsumerSecretKey"`
	TwAccessToken               string        `json:"twitterAccessToken"`
	TwAccessTokenSecret         string        `json:"twitterAccessTokenSecret"`
}
type app struct {
	Config config `json:"config"`
	sync.RWMutex
	LimitQueue *limitQueue `json:"limitQueue"`
	VisitCount int         `json:"visitCount"`
}

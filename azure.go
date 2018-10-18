package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RobustPerception/azure_metrics_exporter/config"
)

// AzureMetricDefinitionResponse represents metric definition response for a given resource from Azure.
type AzureMetricDefinitionResponse struct {
	MetricDefinitionResponses []metricDefinitionResponse `json:"value"`
}
type metricDefinitionResponse struct {
	Dimensions []struct {
		LocalizedValue string `json:"localizedValue"`
		Value          string `json:"value"`
	} `json:"dimensions"`
	ID                   string `json:"id"`
	IsDimensionRequired  bool   `json:"isDimensionRequired"`
	MetricAvailabilities []struct {
		Retention string `json:"retention"`
		TimeGrain string `json:"timeGrain"`
	} `json:"metricAvailabilities"`
	Name struct {
		LocalizedValue string `json:"localizedValue"`
		Value          string `json:"value"`
	} `json:"name"`
	PrimaryAggregationType string `json:"primaryAggregationType"`
	ResourceID             string `json:"resourceId"`
	Unit                   string `json:"unit"`
}

// AzureMetricValueResponse represents a metric value response for a given metric definition.
type AzureMetricValueResponse struct {
	Value []struct {
		Timeseries []struct {
			Data []struct {
				TimeStamp string  `json:"timeStamp"`
				Total     float64 `json:"total"`
				Average   float64 `json:"average"`
				Minimum   float64 `json:"minimum"`
				Maximum   float64 `json:"maximum"`
			} `json:"data"`
		} `json:"timeseries"`
		ID   string `json:"id"`
		Name struct {
			LocalizedValue string `json:"localizedValue"`
			Value          string `json:"value"`
		} `json:"name"`
		Type string `json:"type"`
		Unit string `json:"unit"`
	} `json:"value"`
	APIError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// AzureBatchResponse contains the result of several get metrics requests
type AzureBatchResponse struct {
	Responses []struct {
		HttpStatusCode int                      `json:"httpStatusCode"`
		Headers        map[string]string        `json:"headers"`
		Content        AzureMetricValueResponse `json:"content"`
		ContentLength  int                      `json:"contentLength"`
	} `json:"responses"`
}

// AzureClient represents our client to talk to the Azure api
type AzureClient struct {
	client               *http.Client
	accessToken          string
	accessTokenExpiresOn time.Time
}

// NewAzureClient returns an Azure client to talk the Azure API
func NewAzureClient() *AzureClient {
	return &AzureClient{
		client:               &http.Client{},
		accessToken:          "",
		accessTokenExpiresOn: time.Time{},
	}
}

func (ac *AzureClient) getAccessToken() error {
	target := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/token", sc.C.Credentials.TenantID)
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"resource":      {"https://management.azure.com/"},
		"client_id":     {sc.C.Credentials.ClientID},
		"client_secret": {sc.C.Credentials.ClientSecret},
	}
	resp, err := ac.client.PostForm(target, form)
	if err != nil {
		return fmt.Errorf("Error authenticating against Azure API: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Did not get status code 200, got: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("Error reading body of response: %v", err)
	}
	var data map[string]interface{}
	err = json.Unmarshal(body, &data)
	if err != nil {
		return fmt.Errorf("Error unmarshalling response body: %v", err)
	}
	ac.accessToken = data["access_token"].(string)
	expiresOn, err := strconv.ParseInt(data["expires_on"].(string), 10, 64)
	if err != nil {
		return fmt.Errorf("Error ParseInt of expires_on failed: %v", err)
	}
	ac.accessTokenExpiresOn = time.Unix(expiresOn, 0).UTC()

	return nil
}

// Loop through all specified resource targets and get their respective metric definitions.
func (ac *AzureClient) getMetricDefinitions() (map[string]AzureMetricDefinitionResponse, error) {
	apiVersion := "2018-01-01"
	definitions := make(map[string]AzureMetricDefinitionResponse)

	for _, target := range sc.C.Targets {
		metricsResource := fmt.Sprintf("subscriptions/%s%s", sc.C.Credentials.SubscriptionID, target.Resource)
		metricsTarget := fmt.Sprintf("https://management.azure.com/%s/providers/microsoft.insights/metricDefinitions?api-version=%s", metricsResource, apiVersion)
		req, err := http.NewRequest("GET", metricsTarget, nil)
		if err != nil {
			return nil, fmt.Errorf("Error creating HTTP request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+ac.accessToken)
		resp, err := ac.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Error: %v", err)
		}
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("Error reading body of response: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Error: %v", string(body))
		}

		def := AzureMetricDefinitionResponse{}
		err = json.Unmarshal(body, &def)
		if err != nil {
			return nil, fmt.Errorf("Error unmarshalling response body: %v", err)
		}
		definitions[target.Resource] = def
	}
	return definitions, nil
}

type batchRequest struct {
	Requests []batchURL `json:"requests"`
}
type batchURL struct {
	RelativeURL string `json:"relativeUrl"`
	Method      string `json:"httpMethod"`
}

func (ac *AzureClient) doBatchRequest(urls []string) (*AzureBatchResponse, error) {
	const batchUrl = "https://management.azure.com/batch?api-version=2017-03-01" // "http://localhost:8080/batch?api-version=2017-03-01"
	now := time.Now().UTC()
	refreshAt := ac.accessTokenExpiresOn.Add(-10 * time.Minute)
	if now.After(refreshAt) {
		err := ac.getAccessToken()
		if err != nil {
			return nil, fmt.Errorf("Error refreshing access token: %v", err)
		}
	}

	batch := batchRequest{make([]batchURL, len(urls))}
	for idx, url := range urls {
		batch.Requests[idx] = batchURL{url, "GET"}
	}
	var reqBody bytes.Buffer
	enc := json.NewEncoder(&reqBody)
	enc.SetEscapeHTML(false) // Azure does not handle the &:s becoming \u0026 in the urls
	err := enc.Encode(batch)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", batchUrl, &reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ac.accessToken)

	resp, err := ac.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Error: %v", err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Unable to query metrics API with status code: %d", resp.StatusCode)
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error reading body of response: %v", err)
	}

	var data AzureBatchResponse
	err = json.Unmarshal(respBody, &data)
	if err != nil {
		return nil, fmt.Errorf("Error unmarshalling response body: %v", err)
	}

	return &data, nil
}

func (ac *AzureClient) getMetricURL(metricNames string, target config.Target) string {
	const apiVersion = "2018-01-01"
	metricValueEndpoint := fmt.Sprintf("/subscriptions/%s%s/providers/microsoft.insights/metrics", sc.C.Credentials.SubscriptionID, target.Resource)
	endTime, startTime := GetTimes()

	values := url.Values{}
	if metricNames != "" {
		values.Add("metricnames", metricNames)
	}
	if len(target.Aggregations) > 0 {
		values.Add("aggregation", strings.Join(target.Aggregations, ","))
	} else {
		values.Add("aggregation", "Total,Average,Minimum,Maximum")
	}
	values.Add("timespan", fmt.Sprintf("%s/%s", startTime, endTime))
	values.Add("api-version", apiVersion)

	url := url.URL{
		Path:     metricValueEndpoint,
		RawQuery: values.Encode(),
	}

	return url.String()
}

func (ac *AzureClient) getMetricValue(metricNames string, target config.Target) (AzureMetricValueResponse, error) {
	apiVersion := "2018-01-01"
	now := time.Now().UTC()
	refreshAt := ac.accessTokenExpiresOn.Add(-10 * time.Minute)
	if now.After(refreshAt) {
		err := ac.getAccessToken()
		if err != nil {
			return AzureMetricValueResponse{}, fmt.Errorf("Error refreshing access token: %v", err)
		}
	}

	metricsResource := fmt.Sprintf("subscriptions/%s%s", sc.C.Credentials.SubscriptionID, target.Resource)
	endTime, startTime := GetTimes()

	metricValueEndpoint := fmt.Sprintf("https://management.azure.com/%s/providers/microsoft.insights/metrics", metricsResource)

	req, err := http.NewRequest("GET", metricValueEndpoint, nil)
	if err != nil {
		return AzureMetricValueResponse{}, fmt.Errorf("Error creating HTTP request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+ac.accessToken)

	values := url.Values{}
	if metricNames != "" {
		values.Add("metricnames", metricNames)
	}
	if len(target.Aggregations) > 0 {
		values.Add("aggregation", strings.Join(target.Aggregations, ","))
	} else {
		values.Add("aggregation", "Total,Average,Minimum,Maximum")
	}
	values.Add("timespan", fmt.Sprintf("%s/%s", startTime, endTime))
	values.Add("api-version", apiVersion)

	req.URL.RawQuery = values.Encode()

	log.Printf("GET %s", req.URL)
	resp, err := ac.client.Do(req)
	if err != nil {
		return AzureMetricValueResponse{}, fmt.Errorf("Error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return AzureMetricValueResponse{}, fmt.Errorf("Unable to query metrics API with status code: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return AzureMetricValueResponse{}, fmt.Errorf("Error reading body of response: %v", err)
	}

	var data AzureMetricValueResponse
	err = json.Unmarshal(body, &data)
	if err != nil {
		return AzureMetricValueResponse{}, fmt.Errorf("Error unmarshalling response body: %v", err)
	}

	return data, nil
}

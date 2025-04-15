package sspBC

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v3/adapters"
	"github.com/prebid/prebid-server/v3/config"
	"github.com/prebid/prebid-server/v3/errortypes"
	"github.com/prebid/prebid-server/v3/openrtb_ext"
	"github.com/prebid/prebid-server/v3/util/jsonutil"
)

const (
	adapterVersion              = "5.9"
	impFallbackSize             = "1x1"
	requestTypeStandard         = 1
	requestTypeOneCode          = 2
	requestTypeTest             = 3
	prebidServerIntegrationType = "4"
)

var (
	errSiteNill           = errors.New("site cannot be nill")
	errNotSupportedFormat = errors.New("bid format is not supported")
)

type adapter struct {
	endpoint string
}

// ---------------ADAPTER INTERFACE------------------
// Builder builds a new instance of the sspBC adapter
func Builder(_ openrtb_ext.BidderName, config config.Adapter, server config.Server) (adapters.Bidder, error) {

	bidder := &adapter{
		endpoint: config.Endpoint,
	}

	return bidder, nil
}

func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	formattedRequest, err := formatSspBcRequest(request)
	if err != nil {
		return nil, []error{err}
	}

	requestJSON, err := json.Marshal(formattedRequest)
	if err != nil {
		return nil, []error{err}
	}

	requestURL, err := url.Parse(a.endpoint)
	if err != nil {
		return nil, []error{err}
	}

	// add query parameters to request
	queryParams := requestURL.Query()
	queryParams.Add("bdver", adapterVersion)
	queryParams.Add("inver", prebidServerIntegrationType)
	requestURL.RawQuery = queryParams.Encode()

	requestData := &adapters.RequestData{
		Method: http.MethodPost,
		Uri:    requestURL.String(),
		Body:   requestJSON,
		ImpIDs: openrtb_ext.GetImpIDs(request.Imp),
	}

	return []*adapters.RequestData{requestData}, nil
}

func (a *adapter) MakeBids(internalRequest *openrtb2.BidRequest, externalRequest *adapters.RequestData, externalResponse *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if externalResponse.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	if externalResponse.StatusCode != http.StatusOK {
		err := &errortypes.BadServerResponse{
			Message: fmt.Sprintf("Unexpected status code: %d.", externalResponse.StatusCode),
		}
		return nil, []error{err}
	}

	var response openrtb2.BidResponse
	if err := jsonutil.Unmarshal(externalResponse.Body, &response); err != nil {
		return nil, []error{err}
	}

	bidResponse := adapters.NewBidderResponseWithBidsCapacity(len(internalRequest.Imp))
	bidResponse.Currency = response.Cur

	var errors []error
	for _, seatBid := range response.SeatBid {
		for _, bid := range seatBid.Bid {
			if err := a.impToBid(internalRequest, seatBid, bid, bidResponse); err != nil {
				errors = append(errors, err)
			}
		}
	}

	return bidResponse, errors
}

func (a *adapter) impToBid(internalRequest *openrtb2.BidRequest, seatBid openrtb2.SeatBid, bid openrtb2.Bid,
	bidResponse *adapters.BidderResponse) error {
	var bidType openrtb_ext.BidType

	/*
	  Determine bid type
	  At this moment we only check if bid contains Adm property

	  Later updates will check for video & native data
	*/
	if bid.AdM != "" {
		bidType = openrtb_ext.BidTypeBanner
	} else {
		return errNotSupportedFormat
	}

	// append bid to responses
	bidResponse.Bids = append(bidResponse.Bids, &adapters.TypedBid{
		Bid:     &bid,
		BidType: bidType,
	})

	return nil
}

func formatSspBcRequest(request *openrtb2.BidRequest) (*openrtb2.BidRequest, error) {
	if request.Site == nil {
		return nil, errSiteNill
	}

	siteCopy := *request.Site
	request.Site = &siteCopy

	// add domain info
	if siteURL, err := url.Parse(request.Site.Page); err == nil {
		request.Site.Domain = siteURL.Hostname()
	}

	return request, nil
}

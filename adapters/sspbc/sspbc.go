package sspbc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/mxmCherry/openrtb/v15/openrtb2"
	"github.com/prebid/prebid-server/adapters"
	"github.com/prebid/prebid-server/config"
	"github.com/prebid/prebid-server/errortypes"
	"github.com/prebid/prebid-server/openrtb_ext"
)

const version = "5.6"

// MC payload (for banner ads)
type mcAd struct {
	Id      string             `json:"id"`
	Seat    string             `json:"seat"`
	SeatBid []openrtb2.SeatBid `json:"seatbid"`
}

// Adslot data (oneCode detection)
type adSlotData struct {
	PbSlot string `json:"pbslot"`
	PbSize string `json:"pbsize"`
}

// Banner Template payload
type templatePayload struct {
	SiteId  string `json:"siteid"`
	SlotId  string `json:"slotid"`
	AdLabel string `json:"adlabel"`
	PubId   string `json:"pubid"`
	Page    string `json:"page"`
	Referer string `json:"referer"`
	McAd    string `json:"mcad"`
}

// Ext data in request.imp
type requestImpExt struct {
	Data adSlotData `json:"data"`
}

// Ext data added by proxy
type responseExt struct {
	AdLabel     string `json:"adlabel"`
	PublisherId string `json:"pubid"`
	SiteId      string `json:"siteid"`
	SlotId      string `json:"slotid"`
}

type adapter struct {
	version  string
	endpoint string
	// adslots mapping
	// map key is slot id (as sent and received from proxy)
	adSlots        map[string]adSlotData
	adSizes        map[string]int
	bannerTemplate *template.Template
}

// ---------------ADAPTER INTERFACE------------------
// Builder builds a new instance of the sspBC adapter
func Builder(bidderName openrtb_ext.BidderName, config config.Adapter) (adapters.Bidder, error) {

	// HTML template used to create banner ads
	const bannerHTML = `<html><head><title></title><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><style> body { background-color: transparent; margin: 0; padding: 0; }</style><script> window.rekid = {{.SiteId}}; window.slot = {{.SlotId}}; window.adlabel = '{{.AdLabel}}'; window.pubid = '{{.PubId}}'; window.wp_sn = 'sspbc_go'; window.page = '{{.Page}}'; window.ref = '{{.Referer}}'; window.mcad = JSON.parse(atob('{{.McAd}}'));</script></head><body><div id="c"></div><script async crossorigin nomodule src="//std.wpcdn.pl/wpjslib/wpjslib-inline.js" id="wpjslib"></script><script async crossorigin type="module" src="//std.wpcdn.pl/wpjslib6/wpjslib-inline.js" id="wpjslib6"></script></body></html>`

	bannerTemplate, err := template.New("banner").Parse(bannerHTML)
	if err != nil {
		return nil, err
	}

	bidder := &adapter{
		endpoint:       config.Endpoint,
		version:        version,
		bannerTemplate: bannerTemplate,
	}

	return bidder, nil
}

func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {

	formattedRequest, err := formatSsbcRequest(a, request)
	if err != nil {
		return nil, []error{err}
	}

	requestJSON, err := json.Marshal(formattedRequest)
	if err != nil {
		return nil, []error{err}
	}

	requestUrl, err := url.Parse(a.endpoint)
	if err != nil {
		return nil, []error{err}
	}

	// add query parameters to request
	queryParams := requestUrl.Query()
	queryParams.Add("bdver", a.version) // adapter version
	queryParams.Add("inver", "0")       // integration version (adapter, tag, ...)
	requestUrl.RawQuery = queryParams.Encode()

	requestData := &adapters.RequestData{
		Method: http.MethodPost,
		Uri:    requestUrl.String(),
		Body:   requestJSON,
	}

	return []*adapters.RequestData{requestData}, nil
}

func (a *adapter) MakeBids(internalRequest *openrtb2.BidRequest, externalRequest *adapters.RequestData, externalResponse *adapters.ResponseData) (*adapters.BidderResponse, []error) {

	var errors []error

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
	if err := json.Unmarshal(externalResponse.Body, &response); err != nil {
		return nil, []error{err}
	}

	bidResponse := adapters.NewBidderResponseWithBidsCapacity(len(internalRequest.Imp))
	bidResponse.Currency = response.Cur

	for _, seatBid := range response.SeatBid {
		for _, bid := range seatBid.Bid {
			var bidType openrtb_ext.BidType
			var bidId = bid.ImpID

			/*
			  Determine bid type
			  At this moment we only check if bid contains Adm property

			  Later updates will check for video & native data
			*/
			if bid.AdM != "" {
				bidType = openrtb_ext.BidTypeBanner
			}

			if BidExt, ok := a.adSlots[bidId]; ok {
				var BidIdStored = BidExt.PbSlot
				bid.ImpID = BidIdStored
			}

			// read additional data from proxy
			var BidDataExt responseExt
			if err := json.Unmarshal(bid.Ext, &BidDataExt); err != nil {
				errors = append(errors, err)
			} else {
				var adCreationError error

				/*
					use correct ad creation method for a detected bid type
					right now, we are only creating banner ads
					if type is not detected / supported, throw error
				*/
				if bidType == openrtb_ext.BidTypeBanner {
					bid.AdM, adCreationError = a.createBannerAd(bid, BidDataExt, internalRequest, seatBid.Seat)
				} else {
					adCreationError = fmt.Errorf("bid type is not supported: %s", bidType)
				}

				if adCreationError != nil {
					errors = append(errors, err)
				} else {
					// append bid to responses
					bidResponse.Bids = append(bidResponse.Bids, &adapters.TypedBid{
						Bid:     &bid,
						BidType: bidType,
					})
				}
			}
		}
	}

	return bidResponse, errors
}

func (a *adapter) createBannerAd(bid openrtb2.Bid, ext responseExt, request *openrtb2.BidRequest, seat string) (string, error) {
	var mcad mcAd

	if strings.Contains(bid.AdM, "<!--preformatted-->") {
		// Banner ad is already formatted
		return bid.AdM, nil
	}

	// create McAd payload
	mcad.Id = request.ID
	mcad.Seat = seat
	mcad.SeatBid = make([]openrtb2.SeatBid, 1)
	mcad.SeatBid[0].Bid = make([]openrtb2.Bid, 1)
	mcad.SeatBid[0].Bid[0] = bid
	mcMarshalled, err := json.Marshal(mcad)
	if err != nil {
		return bid.AdM, err
	}

	mcEncoded := base64.URLEncoding.EncodeToString(mcMarshalled)

	bannerData := &templatePayload{
		SiteId:  ext.SiteId,
		SlotId:  ext.SlotId,
		AdLabel: ext.AdLabel,
		PubId:   ext.PublisherId,
		Page:    request.Site.Page,
		Referer: request.Site.Ref,
		McAd:    mcEncoded,
	}

	// Prepare banner html, using template file
	var filledTemplate bytes.Buffer
	if err := a.bannerTemplate.Execute(&filledTemplate, bannerData); err != nil {
		return bid.AdM, err
	}

	return filledTemplate.String(), nil
}

func getImpSize(Imp openrtb2.Imp) string {
	impSize := "1x1"

	if Imp.Banner != nil {
		areaMax := int64(0)
		for _, sizeI := range Imp.Banner.Format {
			areaI := sizeI.W * sizeI.H
			if areaI > areaMax {
				areaMax = areaI
				impSize = fmt.Sprintf("%dx%d", sizeI.W, sizeI.H)
			}
		}
	}

	// default fallback
	return impSize
}

func formatSsbcRequest(a *adapter, request *openrtb2.BidRequest) (*openrtb2.BidRequest, error) {
	var err error
	var siteId string
	var isTest int

	// check if adSlots and adSizes maps are initialized
	if a.adSlots == nil {
		a.adSlots = make(map[string]adSlotData)
	}
	if a.adSizes == nil {
		a.adSizes = make(map[string]int)
	}

	for i, impI := range request.Imp {
		// read ext data for the impression
		var extSSP openrtb_ext.ExtImpSspbc
		var extI = impI.Ext
		var extBidder adapters.ExtImpBidder
		var extData adSlotData

		// Read additional data for this imp.
		// Errors here do not break the flow for this imp, and are ignored
		if err := json.Unmarshal(extI, &extBidder); err == nil {
			_ = json.Unmarshal(extBidder.Bidder, &extSSP)
		}

		// store SiteID
		if extSSP.SiteId != "" {
			siteId = extSSP.SiteId
		}

		// store test info
		if extSSP.IsTest != 0 {
			isTest = 1
		}

		// save current imp.id (adUnit name) as imp.tagid
		impI.TagID = impI.ID

		// if there is a placement id, use it in imp.id
		if extSSP.Id != "" {
			impI.ID = extSSP.Id
		}

		// check imp size and number of times it has been used
		impSize := getImpSize(impI)

		// save slot data
		a.adSizes[impSize] = a.adSizes[impSize] + 1
		if a.adSlots[impI.ID].PbSlot != "" {
			extData = a.adSlots[impI.ID]
		} else {
			extData.PbSlot = impI.TagID
			extData.PbSize = fmt.Sprintf("%s_%d", impSize, a.adSizes[impSize])
			a.adSlots[impI.ID] = extData
		}

		// update bid.ext - send pbslot, pbsize
		// inability to set bid.ext will cause request to be invalid
		var newExtI requestImpExt
		newExtI.Data = extData
		if impI.Ext, err = json.Marshal(newExtI); err != nil {
			return nil, err
		}

		// save updated imp
		request.Imp[i] = impI
	}

	// update site info (ID, of present, and request domain)
	if siteId != "" {
		request.Site.ID = siteId
	}

	// add domain info
	if url, parseError := url.Parse(request.Site.Page); parseError == nil {
		request.Site.Domain = url.Hostname()
	}

	// set TEST Flag
	if isTest == 1 {
		request.Test = 1
	}

	return request, nil
}

package sspbc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/golang/glog"

	"github.com/mxmCherry/openrtb/v15/openrtb2"
	"github.com/prebid/prebid-server/adapters"
	"github.com/prebid/prebid-server/config"
	"github.com/prebid/prebid-server/errortypes"
	"github.com/prebid/prebid-server/openrtb_ext"
)

const version = "5.6"

// MC payload (for banner ads)
type McAd struct {
	Id      string             `json:"id"`
	Seat    string             `json:"seat"`
	SeatBid []openrtb2.SeatBid `json:"seatbid"`
}

// Adslot data (oneCode detection)
type AdSlotData struct {
	PbSlot string `json:"pbslot"`
	PbSize string `json:"pbsize"`
}

// Banner Template payload
type TemplatePayload struct {
	SiteId  string `json:"siteid"`
	SlotId  string `json:"slotid"`
	AdLabel string `json:"adlabel"`
	PubId   string `json:"pubid"`
	Page    string `json:"page"`
	Referer string `json:"referer"`
	McAd    string `json:"mcad"`
}

// Ext data in request.imp
type SsbcRequestImpExt struct {
	Data AdSlotData `json:"data"`
}

// Ext data added by proxy
type SsbcResponseExt struct {
	AdLabel     string `json:"adlabel"`
	PublisherId string `json:"pubid"`
	SiteId      string `json:"siteid"`
	SlotId      string `json:"slotid"`
}

type SspbcAdapter struct {
	version  string
	endpoint string
	// adslots mapping
	// map key is slot id (as sent and received from proxy)
	adSlots        map[string]AdSlotData
	adSizes        map[string]int
	bannerTemplate *template.Template
}

// ---------------ADAPTER INTERFACE------------------
// Builder builds a new instance of the sspBC adapter
func Builder(bidderName openrtb_ext.BidderName, config config.Adapter) (adapters.Bidder, error) {
	// find path to template file
	pathToTemplate := "./adapters/sspbc/bannerTemplate.html"
	workingDir, err := os.Getwd()
	if err == nil && strings.HasSuffix(workingDir, "sspbc") {
		// this is a test running in adapter's directory
		pathToTemplate = "bannerTemplate.html"
	} else if err != nil {
		// this error does not break createBannerAd flow
		glog.Errorf("SSPBC: Cannot get working directory, assuming default path")
	}

	bannerTemplate, err := template.ParseFiles(pathToTemplate)
	if err != nil {
		return nil, err
	}

	bidder := &SspbcAdapter{
		endpoint:       config.Endpoint,
		version:        version,
		bannerTemplate: bannerTemplate,
	}

	return bidder, nil
}

func (a *SspbcAdapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var errors []error

	formattedRequest, err := formatSsbcRequest(a, request)
	if err != nil {
		glog.Errorf("SSPBC: cannot prepare request")
		errors = append(errors, err)
		return nil, errors
	}

	requestJSON, err := json.Marshal(formattedRequest)
	if err != nil {
		glog.Errorf("SSPBC: cannot marshal request")
		errors = append(errors, err)
		return nil, errors
	}

	requestData := &adapters.RequestData{
		Method: "POST",
		Uri:    fmt.Sprintf("%s?bdver=%s&inver=0", a.endpoint, a.version),
		Body:   requestJSON,
	}

	return []*adapters.RequestData{requestData}, nil
}

func (a *SspbcAdapter) MakeBids(internalRequest *openrtb2.BidRequest, externalRequest *adapters.RequestData, externalResponse *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	/*
		  proxy responds with the following format
			{
			"cur": "PLN",
			"id": "...",
			"seatbid": [
				{
					"bid": [
						{
							"adm": "....",
							"adomain": [
								"sspbc-test"
							],
							"crid": "1234",
							"ext": {
								"adlabel": "Reklama",
								"pubid": "431",
								"siteid": "237503",
								"slotid": "005",
								"tagid": "slot"
							},
							"h": 250,
						"id": "...",
								"impid": "005",
								"price": 95.95,
							"w": 300
						}
					],
					"seat": "sspbc-test"
				}
			],
			"sn": "sspbc-test"
			}

		Note - we cannot read site SN, since response.sn is not defined in
		openRTB2.BidResponse structure

		For now we set SN as sspbc_go

		Long term SN should be returned in bid.ext
	*/

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
			var BidType openrtb_ext.BidType
			var BidId = bid.ImpID

			/*
			  here we should make a call to getBidType method, and based on detected type
			  make call to createBannerAd, createVideoAd, createNativeAd methods

			  for now we set it to "banner"
			*/
			BidType = openrtb_ext.BidTypeBanner

			if BidExt, ok := a.adSlots[BidId]; ok {
				var BidIdStored = BidExt.PbSlot
				bid.ImpID = BidIdStored
			} else {
				glog.Errorf("SSPBC: BidExt for this bid.impid not found - %s", BidId)
			}

			// read additional data from proxy
			var BidDataExt SsbcResponseExt
			if err := json.Unmarshal(bid.Ext, &BidDataExt); err != nil {
				glog.Errorf("SSPBC: cannot unmarshal Bid Ext data")
				errors = append(errors, err)
			} else {
				var adCreationError error

				// Prepare ads (using different methods for banner, native, video)

				// BANNER
				bid.AdM, adCreationError = a.createBannerAd(bid, BidDataExt, internalRequest, seatBid.Seat)

				if adCreationError != nil {
					glog.Errorf("SSPBC: cannot format creative")
					errors = append(errors, err)
				} else {
					// append bid to responses
					bidResponse.Bids = append(bidResponse.Bids, &adapters.TypedBid{
						Bid:     &bid,
						BidType: BidType,
					})
				}
			}
		}
	}

	return bidResponse, errors
}

func (a *SspbcAdapter) createBannerAd(bid openrtb2.Bid, ext SsbcResponseExt, request *openrtb2.BidRequest, seat string) (string, error) {
	var mcad McAd

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
		glog.Errorf("SSPBC: Cannot Marshal mcad!")
		return bid.AdM, err
	}

	mcEncoded := base64.URLEncoding.EncodeToString(mcMarshalled)

	bannerData := &TemplatePayload{
		SiteId:  ext.SiteId,
		SlotId:  ext.SlotId,
		AdLabel: ext.AdLabel,
		PubId:   ext.PublisherId,
		Page:    request.Site.Page,
		Referer: request.Site.Ref,
		McAd:    mcEncoded,
	}

	/*
		Prepare banner html, using template file

		Note: Prebidserver bidders have access only to gdpr data in user ext, which is not what we need (as mcad uses prebid.js gdpr format)
		Therefore, we are not creating window.gdpr. This will force banner creative to execute it's own call to TCF2
	*/

	var filledTemplate bytes.Buffer
	if err := a.bannerTemplate.Execute(&filledTemplate, bannerData); err != nil {
		glog.Errorf("SSPBC: Cannot execute banner template")
		return bid.AdM, err
	}

	/*
		byteTemplate := []byte(filledTemplate.String())
		fmt.Println(base64.StdEncoding.EncodeToString(byteTemplate)) */

	return filledTemplate.String(), nil
}

func getImpSize(Imp openrtb2.Imp) string {
	if Imp.Video != nil {
		return fmt.Sprintf("%dx%d", Imp.Video.W, Imp.Video.H)
	}

	if Imp.Banner != nil {
		sb := strings.Builder{}
		areaMax := int64(0)
		sb.WriteString("1x1")
		for _, sizeI := range Imp.Banner.Format {
			areaI := sizeI.W * sizeI.H
			if areaI > areaMax {
				areaMax = areaI
				sb.Reset()
				sb.WriteString(fmt.Sprintf("%dx%d", sizeI.W, sizeI.H))
			}
		}
		return sb.String()
	}

	// default fallback
	return "1x1"
}

func formatSsbcRequest(a *SspbcAdapter, request *openrtb2.BidRequest) (*openrtb2.BidRequest, error) {
	var err error
	var siteId string
	var isTest int

	// check if adSlots and adSizes maps are initialized
	if a.adSlots == nil {
		a.adSlots = make(map[string]AdSlotData)
	}
	if a.adSizes == nil {
		a.adSizes = make(map[string]int)
	}

	for i, impI := range request.Imp {
		// read ext data for the impression
		var extSSP openrtb_ext.ExtImpSspbc
		var extI = impI.Ext
		var extBidder adapters.ExtImpBidder
		var extData AdSlotData

		// Read Ext data for this imp. Note that errors here do not break the flow for this imp
		if extBidderErr := json.Unmarshal(extI, &extBidder); extBidderErr != nil {
			glog.Errorf("SSPBC: Error reading bid.ext %s", extBidderErr)
		} else {
			if extSSPErr := json.Unmarshal(extBidder.Bidder, &extSSP); extSSPErr != nil {
				glog.Errorf("SSPBC: Error reading bidder-specific ext data %s", extSSPErr)
			}
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
		var newExtI SsbcRequestImpExt
		newExtI.Data = extData
		if impI.Ext, err = json.Marshal(newExtI); err != nil {
			glog.Errorf("SSPBC: Cannot set ext data for the imp. This is a fatal error %s", err)
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
	} else {
		glog.Errorf("SSPBC: Cannot parse site url %s", parseError)
	}

	// set TEST Flag
	if isTest == 1 {
		request.Test = 1
	}

	return request, nil
}
package provider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPhoneCountriesNamesAndShapes(t *testing.T) {
	for _, body := range []string{
		`{"":{"id":"","eng":"All"},"187":{"id":187,"eng":"USA","chn":"美国"},"36":{"id":"36","eng":"Canada"},"0":{"id":0,"rus":"Россия"}}`,
		`[{"id":187,"eng":"USA","chn":"美国"},{"id":"36","eng":"Canada"},{"id":0,"rus":"Россия"}]`,
	} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if q := r.URL.Query(); r.URL.Path != "/stubs/handler_api.php" || q.Get("action") != "getCountries" || len(q) != 2 || q.Get("api_key") != testKey {
				t.Error("unexpected country catalogue request")
			}
			fmt.Fprint(w, body)
		})
		got, err := client.PhoneCountries(context.Background())
		want := []PhoneCountry{{"0", "Россия"}, {"36", "Canada"}, {"187", "美国"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("countries = %+v, %v; want %+v", got, err, want)
		}
	}
}

func TestPhoneChannelsExactFilteringAndDocumentedQueries(t *testing.T) {
	var prices, countryCalls, topCalls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/stubs/handler_api.php" || q.Get("api_key") != testKey {
			t.Error("unexpected catalogue request")
		}
		switch q.Get("action") {
		case "getPricesV3":
			prices.Add(1)
			if q.Get("country") != "" || q.Get("service") != "dr" || len(q) != 3 {
				t.Error("multiple countries must be filtered locally; only service sent to upstream")
			}
			fmt.Fprint(w, `{
			"36":{"other":{"2":{"price":0.01,"count":99,"provider_id":2}},"dr":{
			 "100":{"price":"0.15000000","count":"4","provider_id":100},
			 "101":{"price":0.15000001,"count":10,"provider_id":101},
			 "102":{"price":0.14999999,"count":2,"provider_id":"102","tier":"Silver"},
			 "103":{"price":0.01,"count":0,"provider_id":103},
			 "104":{"price":0.01,"count":1,"provider_id":999},
			 "105":{"price":"1/10","count":1,"provider_id":105},
			 "106":{"price":0.1,"count":1.2,"provider_id":106},
			 "107":{"price":0.1,"count":2147483648,"provider_id":107},
			 "108":{"price":0.1,"count":3,"provider_id":108,"tier":3},
			 "109":{"price":0.1,"count":3,"provider_id":109,"rank":"bronze"},
			 "110":{"price":0.1,"count":3,"provider_id":110,"quality":"diamond"},
			 "111":{"price":0.1,"count":3,"provider_id":111}
			}},
			"187":{"dr":{"200":{"price":"0.149","count":5,"provider_id":"200"}}},
			"1":{"dr":{"300":{"price":0.01,"count":99,"provider_id":300}}}
			}`)
		case "getCountries":
			countryCalls.Add(1)
			fmt.Fprint(w, `{"36":{"id":36,"eng":"Canada","chn":"加拿大"},"187":{"id":187,"eng":"United States","chn":"美国"}}`)
		case "getTopCountriesByService":
			topCalls.Add(1)
			fmt.Fprint(w, `{"canada":{"100":{"price":0.15,"count":4}},"united-states":{"200":{"price":0.149,"count":5}}}`)
		default:
			t.Error("catalogue issued undocumented or mutating action")
		}
	})
	req := phoneRequest()
	req.Country, req.MaxPrice, req.TTL = "36,187,36", "0.15", 0
	got, err := client.PhoneChannels(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := []PhoneChannel{
		{"36", "加拿大", "100", "0.15000000", 4, "gold", "provided"},
		{"36", "加拿大", "102", "0.14999999", 2, "silver", "provided"},
		{"187", "美国", "200", "0.149", 5, "gold", "provided"},
		{"36", "加拿大", "108", "0.1", 3, "unknown", "unavailable"},
		{"36", "加拿大", "109", "0.1", 3, "bronze", "provided"},
		{"36", "加拿大", "110", "0.1", 3, "unknown", "unavailable"},
		{"36", "加拿大", "111", "0.1", 3, "unknown", "not_provided"},
	}
	if !reflect.DeepEqual(got, want) || prices.Load() != 1 || countryCalls.Load() != 1 || topCalls.Load() != 1 {
		t.Fatalf("channels = %+v; want %+v; calls=%d,%d,%d", got, want, prices.Load(), countryCalls.Load(), topCalls.Load())
	}
}

func TestPhoneChannelsSingleCountryAndOptionalMetadataFailure(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("action") == "getPricesV3" {
			if q.Get("country") != "187" || q.Get("service") != "dr" || len(q) != 4 {
				t.Error("single country should be requested explicitly")
			}
			fmt.Fprint(w, `{"187":{"dr":{"42":{"provider_id":42,"price":0.2,"count":7}}}}`)
		} else {
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
		}
	})
	got, err := client.PhoneChannels(context.Background(), phoneRequest())
	if err != nil || len(got) != 1 || got[0].CountryName != "187" || got[0].Tier != "unknown" || got[0].TierStatus != "unavailable" {
		t.Fatalf("valid quotes must survive metadata outage: %+v, %v", got, err)
	}
}

func TestPhoneCatalogErrorsAreReadOnlyAndSanitized(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{"BAD_KEY", "bad_key"}, {"BAD_SERVICE", "upstream_configuration"},
		{`{"error":"BAD_KEY"}`, "bad_key"},
		{`{"error":"` + testKey + ` https://private.example/?api_key=secret"}`, "invalid_response"},
		{`{"status":0}`, "invalid_response"}, {`null`, "invalid_response"},
		{`[]`, "invalid_response"}, {`<html>error</html>`, "invalid_response"},
	} {
		t.Run(tc.body[:min(12, len(tc.body))], func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
			_, err := client.PhoneChannels(context.Background(), phoneRequest())
			typedError(t, err, tc.code, false)
		})
	}
	for _, body := range []string{`{}`, `NO_NUMBERS`} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		got, err := client.PhoneChannels(context.Background(), phoneRequest())
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty stock = %+v, %v", got, err)
		}
	}
}

func TestPhoneCatalogRejectsBadFiltersBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	requests := []Request{{}, emailRequest()}
	for _, value := range []string{"*", "1,", "-1", "+1", "1,2&service=x", "01", "1e2", "4294967296", strings.Repeat("1", 4097)} {
		r := phoneRequest()
		r.Country = value
		requests = append(requests, r)
	}
	for _, value := range []string{"", "0", "-1", "1/10", "NaN", "Infinity", "1e-1"} {
		r := phoneRequest()
		r.MaxPrice = value
		requests = append(requests, r)
	}
	for _, req := range requests {
		_, err := client.PhoneChannels(context.Background(), req)
		typedError(t, err, "invalid_request", false)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid request contacted provider %d times", calls.Load())
	}
}

func TestPhoneAllocationPinsOneProviderAndPrice(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if q.Get("action") != "getNumberV2" || q.Get("country") != "187" || q.Get("providerIds") != "3243" || q.Get("maxPrice") != "0.11900000" || len(q) != 6 {
			t.Error("allocation lost pinned provider/country/maximum price")
		}
		fmt.Fprint(w, `{"activationId":123,"phoneNumber":"12025550123"}`)
	})
	req := phoneRequest()
	req.ProviderID, req.MaxPrice = "3243", "0.11900000"
	if _, err := client.Allocate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1,2", "-1", "0", "1&service=x", "01", "4294967296"} {
		req.ProviderID = id
		_, err := client.Allocate(context.Background(), req)
		typedError(t, err, "invalid_request", false)
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid channel performed allocation; calls=%d", calls.Load())
	}
}

func TestGoldEnrichmentNeverGuessCountryOrDemoteMissing(t *testing.T) {
	channels := []PhoneChannel{
		{Country: "36", ProviderID: "42", Tier: "unknown"},
		{Country: "187", ProviderID: "42", Tier: "unknown"},
		{Country: "36", ProviderID: "43", Tier: "bronze"},
		{Country: "36", ProviderID: "44", Tier: "unknown"},
	}
	countries := []catalogCountry{{PhoneCountry: PhoneCountry{ID: "36"}, English: "Canada"}, {PhoneCountry: PhoneCountry{ID: "187"}, English: "USA"}}
	addGoldPhoneTiers(channels, []byte(`{"canada":{"42":{"price":0.1,"count":1},"43":{"price":0.1,"count":1}},"ambiguous":{"44":{"price":0.1,"count":1}}}`), countries)
	if channels[0].Tier != "gold" || channels[1].Tier != "unknown" || channels[2].Tier != "bronze" || channels[3].Tier != "unknown" {
		t.Fatalf("unproven quality inferred: %+v", channels)
	}
}

func TestPhoneRatingDistinguishesMissingFromMetadataFailure(t *testing.T) {
	for _, tc := range []struct {
		name, goldBody, unknownStatus string
		goldHTTP                      int
	}{
		{"partial valid list", `{"united-states":{"99":{"price":0.1,"count":3}}}`, "not_provided", 200},
		{"valid empty list", `{}`, "not_provided", 200},
		{"network failure", ``, "unavailable", 503},
		{"unrecognized response", `{"status":true}`, "unavailable", 200},
		{"upstream error", `{"error":"BAD_SERVICE"}`, "unavailable", 200},
		{"country mapping missing", `{"unmapped-country":{"42":{"price":0.1,"count":3}}}`, "unavailable", 200},
		{"malformed provider", `{"united-states":{"42":{"cost":0.1}}}`, "unavailable", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Query().Get("action") {
				case "getPricesV3":
					fmt.Fprint(w, `{"187":{"dr":{"42":{"provider_id":42,"price":0.1,"count":3},"43":{"provider_id":43,"price":0.1,"count":3,"tier":"silver"}}}}`)
				case "getCountries":
					fmt.Fprint(w, `{"187":{"id":187,"eng":"United States","chn":"美国"}}`)
				case "getTopCountriesByService":
					w.WriteHeader(tc.goldHTTP)
					fmt.Fprint(w, tc.goldBody)
				default:
					t.Error("metadata lookup issued unexpected action")
				}
			})
			got, err := client.PhoneChannels(context.Background(), phoneRequest())
			if err != nil || len(got) != 2 {
				t.Fatalf("metadata failure hid valid quotes: %+v, %v", got, err)
			}
			if got[0].Tier != "unknown" || got[0].TierStatus != tc.unknownStatus || got[1].Tier != "silver" || got[1].TierStatus != "provided" {
				t.Fatalf("rating source lost: %+v", got)
			}
		})
	}
}

func TestPhoneCountryMetadataCannotConsumeGoldRequestBudget(t *testing.T) {
	countryStarted, goldStarted := make(chan struct{}), make(chan struct{})
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getPricesV3":
			fmt.Fprint(w, `{"187":{"dr":{"42":{"provider_id":42,"price":0.1,"count":3}}}}`)
		case "getCountries":
			close(countryStarted)
			select {
			case <-goldStarted:
				fmt.Fprint(w, `{"187":{"id":187,"eng":"United States","chn":"美国"}}`)
			case <-r.Context().Done():
			}
		case "getTopCountriesByService":
			select {
			case <-countryStarted:
				close(goldStarted)
				fmt.Fprint(w, `{"united-states":{"42":{"price":0.1,"count":3}}}`)
			case <-r.Context().Done():
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := client.PhoneChannels(ctx, phoneRequest())
	if err != nil || len(got) != 1 || got[0].Tier != "gold" || got[0].TierStatus != "provided" || got[0].CountryName != "美国" {
		t.Fatalf("country lookup prevented independent Gold enrichment: %+v, %v", got, err)
	}
}

func TestNumericGoldCountrySurvivesCountryNameOutage(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getPricesV3":
			fmt.Fprint(w, `{"187":{"dr":{"42":{"provider_id":42,"price":0.1,"count":3}}}}`)
		case "getCountries":
			http.Error(w, "country lookup unavailable", 503)
		case "getTopCountriesByService":
			fmt.Fprint(w, `{"187":{"42":{"price":0.1,"count":3}}}`)
		}
	})
	got, err := client.PhoneChannels(context.Background(), phoneRequest())
	if err != nil || len(got) != 1 || got[0].Tier != "gold" || got[0].TierStatus != "provided" || got[0].CountryName != "187" {
		t.Fatalf("numeric country rating depends on optional names: %+v, %v", got, err)
	}
}

func TestDemoPhoneCatalogueUsesSameFilters(t *testing.T) {
	d := NewDemo().(PhoneCatalogClient)
	got, err := d.PhoneChannels(context.Background(), Request{Kind: "phone", Service: "dr", Country: "36,187", MaxPrice: "0.15"})
	if err != nil || len(got) != 4 {
		t.Fatalf("demo filtered catalogue = %+v, %v", got, err)
	}
	for _, c := range got {
		if c.Country == "0" || c.Tier == "gold" || c.Count <= 0 {
			t.Fatalf("demo did not apply catalogue filter: %+v", c)
		}
	}
	all, err := d.PhoneChannels(context.Background(), Request{Kind: "phone", Service: "dr", MaxPrice: "0.20"})
	if err != nil || len(all) != 9 {
		t.Fatalf("demo all countries = %+v, %v", all, err)
	}
}

package v2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/armosec/kubescape/cautils"
	"github.com/armosec/kubescape/cautils/getter"
	"github.com/armosec/kubescape/cautils/logger"
	"github.com/armosec/kubescape/cautils/logger/helpers"
	uuid "github.com/satori/go.uuid"

	"github.com/armosec/opa-utils/reporthandling"
	"github.com/armosec/opa-utils/reporthandling/results/v1/resourcesresults"
	reporthandlingv2 "github.com/armosec/opa-utils/reporthandling/v2"
)

const MAX_REPORT_SIZE = 2097152 // 2 MB

type ReportEventReceiver struct {
	httpClient         *http.Client
	clusterName        string
	customerGUID       string
	eventReceiverURL   *url.URL
	token              string
	customerAdminEMail string
	message            string
}

func NewReportEventReceiver(tenantConfig *cautils.ConfigObj) *ReportEventReceiver {
	return &ReportEventReceiver{
		httpClient:         &http.Client{},
		clusterName:        tenantConfig.ClusterName,
		customerGUID:       tenantConfig.AccountID,
		token:              tenantConfig.Token,
		customerAdminEMail: tenantConfig.CustomerAdminEMail,
	}
}

func (report *ReportEventReceiver) ActionSendReport(opaSessionObj *cautils.OPASessionObj) error {
	finalizeReport(opaSessionObj)

	if report.customerGUID == "" {
		report.message = "WARNING: Failed to publish results. Reason: Unknown accout ID. Run kubescape with the '--account <account ID>' flag. Contact ARMO team for more details"
		return nil
	}
	if report.clusterName == "" {
		report.message = "WARNING: Failed to publish results because the cluster name is Unknown. If you are scanning YAML files the results are not submitted to the Kubescape SaaS"
		return nil
	}
	opaSessionObj.Report.ReportID = uuid.NewV4().String()
	opaSessionObj.Report.CustomerGUID = report.customerGUID
	opaSessionObj.Report.ClusterName = report.clusterName

	if err := report.prepareReport(opaSessionObj.Report); err != nil {
		report.message = err.Error()
	} else {
		report.generateMessage()
	}
	logger.L().Debug("", helpers.String("account ID", report.customerGUID))

	return nil
}

func (report *ReportEventReceiver) SetCustomerGUID(customerGUID string) {
	report.customerGUID = customerGUID
}

func (report *ReportEventReceiver) SetClusterName(clusterName string) {
	report.clusterName = cautils.AdoptClusterName(clusterName) // clean cluster name
}

func (report *ReportEventReceiver) prepareReport(postureReport *reporthandlingv2.PostureReport) error {
	report.initEventReceiverURL()
	host := hostToString(report.eventReceiverURL, postureReport.ReportID)

	cautils.StartSpinner()
	defer cautils.StopSpinner()

	reportCounter := 0

	// send resources
	if err := report.sendResources(host, postureReport, &reportCounter, false); err != nil {
		return err
	}
	// reportCounter++

	// // send results
	// if err := report.sendResults(host, postureReport, &reportCounter, true); err != nil {
	// 	return err
	// }

	return nil
}

func (report *ReportEventReceiver) sendResources(host string, postureReport *reporthandlingv2.PostureReport, reportCounter *int, isLastReport bool) error {
	splittedPostureReport := setSubReport(postureReport)
	counter := 0

	for _, v := range postureReport.Resources {
		r, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("failed to unmarshal resource '%s', reason: %v", v.ResourceID, err)
		}

		if counter+len(r) >= MAX_REPORT_SIZE && len(splittedPostureReport.Resources) > 0 {

			// send report
			if err := report.sendReport(host, splittedPostureReport, *reportCounter, false); err != nil {
				return err
			}
			*reportCounter++

			// delete resources
			splittedPostureReport.Resources = []reporthandling.Resource{}
			splittedPostureReport.Results = []resourcesresults.Result{}

			// restart counter
			counter = 0
		}

		counter += len(r)
		splittedPostureReport.Resources = append(splittedPostureReport.Resources, v)
	}

	for _, v := range postureReport.Results {
		r, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("failed to unmarshal resource '%s', reason: %v", v.GetResourceID(), err)
		}

		if counter+len(r) >= MAX_REPORT_SIZE && len(splittedPostureReport.Results) > 0 {

			// send report
			if err := report.sendReport(host, splittedPostureReport, *reportCounter, false); err != nil {
				return err
			}
			*reportCounter++

			// delete results
			splittedPostureReport.Results = []resourcesresults.Result{}
			splittedPostureReport.Resources = []reporthandling.Resource{}

			// restart counter
			counter = 0
		}

		counter += len(r)
		splittedPostureReport.Results = append(splittedPostureReport.Results, v)
	}

	return report.sendReport(host, splittedPostureReport, *reportCounter, true)
}

// func (report *ReportEventReceiver) sendResults(host string, postureReport *reporthandlingv2.PostureReport, reportCounter *int, isLastReport bool) error {
// 	splittedPostureReport := setSubReport(postureReport)
// 	counter := 0

// 	for _, v := range postureReport.Results {
// 		r, err := json.Marshal(v)
// 		if err != nil {
// 			return fmt.Errorf("failed to unmarshal resource '%s', reason: %v", v.GetResourceID(), err)
// 		}

// 		if counter+len(r) >= MAX_REPORT_SIZE && len(splittedPostureReport.Resources) > 0 {

// 			// send report
// 			if err := report.sendReport(host, splittedPostureReport, *reportCounter, false); err != nil {
// 				return err
// 			}
// 			*reportCounter++

// 			// delete results
// 			splittedPostureReport.Results = []resourcesresults.Result{}

// 			// restart counter
// 			counter = 0
// 		}

// 		counter += len(r)
// 		splittedPostureReport.Results = append(splittedPostureReport.Results, v)
// 	}

// 	return report.sendReport(host, splittedPostureReport, *reportCounter, isLastReport)
// }

func (report *ReportEventReceiver) sendReport(host string, postureReport *reporthandlingv2.PostureReport, counter int, isLastReport bool) error {
	postureReport.PaginationInfo = reporthandlingv2.PaginationMarks{
		ReportNumber: counter,
		IsLastReport: isLastReport,
	}
	reqBody, err := json.Marshal(postureReport)
	if err != nil {
		return fmt.Errorf("in 'sendReport' failed to json.Marshal, reason: %v", err)
	}
	msg, err := getter.HttpPost(report.httpClient, host, nil, reqBody)
	if err != nil {
		return fmt.Errorf("%s, %v:%s", host, err, msg)
	}
	return err
}

func (report *ReportEventReceiver) generateMessage() {

	u := url.URL{}
	u.Scheme = "https"
	u.Host = getter.GetArmoAPIConnector().GetFrontendURL()

	if report.customerAdminEMail != "" { // data has been submitted
		u.Path = fmt.Sprintf("configuration-scanning/%s", report.clusterName)
	} else {
		u.Path = "account/sign-up"
		q := u.Query()
		q.Add("invitationToken", report.token)
		q.Add("customerGUID", report.customerGUID)

		u.RawQuery = q.Encode()
	}

	sep := "~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~\n"
	report.message = sep
	report.message += "   << WOW! Now you can see the scan results on the web >>\n\n"
	report.message += fmt.Sprintf("   %s\n", u.String())
	report.message += sep

}

func (report *ReportEventReceiver) DisplayReportURL() {
	cautils.InfoTextDisplay(os.Stderr, fmt.Sprintf("\n\n%s\n\n", report.message))
}

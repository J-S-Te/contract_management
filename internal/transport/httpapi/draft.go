package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func (h *Handler) updateContractDraft(c *gin.Context) {
	var body struct {
		createContractRequest
		ExpectedVersion uint64 `json:"expected_version"`
	}
	if !decode(c, &body) {
		return
	}
	updated, err := h.service.UpdateContractDraft(c.Request.Context(), principal(c), c.Param("contractID"), body.ExpectedVersion, contract.Contract{
		Number: body.Number, Title: body.Title, Type: body.ContractType, ServiceType: body.ServiceType,
		OpportunityID: body.OpportunityID, OpportunityName: body.OpportunityName, CRMCustomerID: body.CRMCustomerID,
		CustomerName: body.CustomerName, CustomerAddress: body.CustomerAddress, CustomerContact: body.CustomerContact, CustomerPhone: body.CustomerPhone,
		Systems: body.Systems, ServiceItems: body.ServiceItems, CustomerCreditLevel: body.CustomerCreditLevel,
		AmountMinor: body.AmountMinor, Currency: body.Currency, Content: body.Content, TemplateID: body.TemplateID,
		TemplateValues: body.TemplateValues, StartDate: body.StartDate, EndDate: body.EndDate,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, updated)
}

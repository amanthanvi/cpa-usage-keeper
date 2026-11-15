package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"cpa-usage-keeper/internal/pricing"
	"cpa-usage-keeper/internal/service"
	servicedto "cpa-usage-keeper/internal/service/dto"
	"github.com/gin-gonic/gin"
)

func registerPricingAddMissingRoute(router gin.IRoutes, provider service.PricingProvider) {
	router.POST("/pricing/models/add-missing", func(c *gin.Context) {
		adder, ok := provider.(service.PricingAddMissingProvider)
		if !ok {
			writeCompletePricingError(c, http.StatusServiceUnavailable, "add_missing_unavailable", "Add-missing pricing is unavailable", nil)
			return
		}
		var input servicedto.AddMissingPricingModelsRequest
		decoder := json.NewDecoder(c.Request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeCompletePricingDecodeError(c, err)
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			writeCompletePricingError(c, http.StatusBadRequest, "invalid_request", "Invalid model configuration", nil)
			return
		}
		response, err := adder.AddMissingPricingModels(c.Request.Context(), input)
		if err == nil {
			c.JSON(http.StatusOK, response)
			return
		}
		switch {
		case errors.Is(err, service.ErrPricingRevisionConflict):
			writeCompletePricingError(c, http.StatusConflict, "revision_conflict", "Pricing revision has changed", nil)
		case errors.Is(err, service.ErrPricingModelExists):
			writeCompletePricingError(c, http.StatusConflict, "model_exists", "Model pricing already exists", nil)
		case errors.Is(err, service.ErrPricingBusy), errors.Is(err, service.ErrInvalidPricingInput):
			writeCompletePricingServiceError(c, err)
		default:
			var conflict *pricing.BranchConflictError
			if errors.As(err, &conflict) {
				writeCompletePricingServiceError(c, err)
				return
			}
			// Do not log database errors or request fields on this command.
			writeCompletePricingError(c, http.StatusInternalServerError, "internal_error", "Internal server error", nil)
		}
	})
}

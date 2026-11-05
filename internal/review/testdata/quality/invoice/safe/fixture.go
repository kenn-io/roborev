package fixture

type Invoice struct {
	Tenant string
	Amount int
}

// ReadInvoice returns the amount only when the invoice belongs to tenant.
func ReadInvoice(invoices map[string]Invoice, tenant, id string) (int, bool) {
	scoped := make(map[string]Invoice)
	for key, invoice := range invoices {
		if invoice.Tenant == tenant {
			scoped[key] = invoice
		}
	}
	invoice, ok := scoped[id]
	if !ok {
		return 0, false
	}
	return invoice.Amount, true
}

package fixture

type Invoice struct {
	Tenant string
	Amount int
}

// ReadInvoice returns the amount only when the invoice belongs to tenant.
func ReadInvoice(invoices map[string]Invoice, tenant, id string) (int, bool) {
	invoice, ok := invoices[id]
	if !ok || invoice.Tenant != tenant {
		return 0, false
	}
	return invoice.Amount, true
}

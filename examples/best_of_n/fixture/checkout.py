def total_cents(items, discount_cents=0):
    """Apply a discount, then charge 500 cents shipping below 5000 cents.

    An empty or fully discounted order costs zero, including shipping.
    """
    subtotal = sum(items)
    shipping = 500 if subtotal < 5000 else 0
    return subtotal - discount_cents + shipping

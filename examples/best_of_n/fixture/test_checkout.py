import unittest
from checkout import total_cents


class CheckoutTests(unittest.TestCase):
    def test_small_order(self):
        self.assertEqual(total_cents([1200, 1800]), 3500)

    def test_free_shipping_boundary(self):
        self.assertEqual(total_cents([2500, 2500]), 5000)

    def test_discount_changes_shipping(self):
        self.assertEqual(total_cents([3000, 2500], 1000), 5000)

    def test_discount_above_subtotal(self):
        self.assertEqual(total_cents([1000], 2000), 0)

    def test_empty_order(self):
        self.assertEqual(total_cents([]), 0)

    def test_fully_discounted(self):
        self.assertEqual(total_cents([1000], 1000), 0)


if __name__ == "__main__":
    unittest.main()

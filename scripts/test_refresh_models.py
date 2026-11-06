import unittest
from refresh_models import refresh


def row(stale=False, variants=False, unavailable=False):
    catalog = {"channel": "cli", "fetched_at": "old" if stale else "new", "stale": stale}
    result = {"id": "test", "owned_by": "workbuddy"}
    if unavailable:
        result["catalog"] = {"stale": True, "availability": "metadata_unavailable"}
    elif variants:
        result["catalog_variants"] = [{"catalog": dict(catalog)}, {"catalog": dict(catalog)}]
    else:
        result["catalog"] = catalog
    return result


class RefreshTests(unittest.TestCase):
    def fixture(self, before, after):
        self.patched = False
        def call(route, key="management_key", data=None):
            if route == "/v0/management/plugins":
                return {"plugins": [{"id": "workbuddy", "effective_enabled": True,
                                     "metadata": {"version": "0.3.5"}}]}
            if route == "/v1/models":
                return {"data": after if self.patched else before}
            if data:
                self.patched = True
                return {}
            if route == "/v0/management/auth-files":
                return {"files": [{"name": "a.json", "type": "workbuddy"},
                                  {"name": "b.json", "type": "workbuddy"}]}
            return {"models": [{"id": "test"}]}
        return call

    def test_new_version_and_multi_account(self):
        result = refresh(self.fixture([row(True, True)], [row(False, True)]))
        self.assertEqual(result["status"], "refreshed")
        self.assertEqual(result["catalog_count"], 2)
        self.assertEqual([x["model_count"] for x in result["accounts"]], [1, 1])

    def test_fresh_cache_is_not_invalidated(self):
        result = refresh(self.fixture([row()], []))
        self.assertEqual(result["status"], "fresh")
        self.assertFalse(self.patched)

    def test_unavailable_is_partial(self):
        result = refresh(self.fixture([row(unavailable=True)], [row(unavailable=True)]))
        self.assertEqual(result["status"], "partial")
        self.assertEqual(result["unavailable_count"], 1)

    def test_no_accounts(self):
        self.assertEqual(refresh(self.fixture([], []))["status"], "skipped")

    def test_old_plugin_without_catalog_is_skipped(self):
        self.assertEqual(refresh(self.fixture([{"id": "test", "owned_by": "workbuddy"}], []))["status"], "skipped")


if __name__ == "__main__":
    unittest.main()

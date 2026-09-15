# Media fixtures live here.
#
# Generate the deterministic test fixture with:
#     go run ./scripts/makefixture.go
#
# Fixture content is a repeating byte pattern (index % 251) so a range
# request can be verified against exact file offsets rather than merely
# checking a length.
#
# bench/fixtures/ is not committed (see .gitignore): fixtures are
# generated, not stored, so the repository stays small.

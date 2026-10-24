# Test fixtures

`wp-rest/` is a recording of the public WordPress REST API of
https://wordpress.org/news/ (see `meta.json`), with hosts rewritten to
`wp.example`. The tests serve it from a local server, so they never touch the
network.

The recorded post content belongs to its authors and is included only as test
input; it is not covered by this repository's MIT license.

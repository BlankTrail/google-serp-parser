-- The road a profile's ports take to their addresses.
--
-- A first hop is a SOCKS5 proxy, or one of the service's gateways, that a
-- port's traffic goes through before the address it is on. It is kept as the
-- text the program reads it from: empty for none — the port goes to its address
-- directly, as every port did before this column — "gw:" and a gateway's name,
-- or the proxy's address whole, login and password included, the way an
-- address from a list is kept.
ALTER TABLE proxy_profiles ADD COLUMN first_hop TEXT NOT NULL DEFAULT '';

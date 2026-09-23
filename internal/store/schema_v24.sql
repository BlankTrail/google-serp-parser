-- How a profile's ports resolve the names they are asked for, once they resolve
-- them at all, and the resolvers named where that is the answer.
--
-- "delegate" for every profile, the ones already written included: the name is
-- handed to the proxy and resolved by whatever the exit itself uses, which is
-- the one answer that cannot disagree with where the traffic comes out, and it
-- costs no round trip of its own. Every other strategy asks a resolver before
-- the request can start, and on a list reached through a first hop that ask
-- goes down the whole chain.
--
-- The column beside it holds the resolvers an operator names, one per line, and
-- means nothing under any other strategy.
ALTER TABLE proxy_profiles ADD COLUMN resolver TEXT NOT NULL DEFAULT 'delegate';
ALTER TABLE proxy_profiles ADD COLUMN custom_resolvers TEXT NOT NULL DEFAULT '';

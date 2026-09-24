-- Names are kept away from the proxy only where a profile says so.
--
-- The step before this one turned it on for every profile. The operator chose
-- the other way round: the service's own resolving with its fallback to the
-- name is the more dependable default, and a profile on a provider that refuses
-- names — the residential gateway that refused the host Google's reCAPTCHA
-- script comes from — is the one to turn it on for. The switch had been on for
-- a few hours when this step was written, on the value the step before set
-- rather than on anybody's choice, so every profile goes back to off.
UPDATE proxy_profiles SET vdns_strict_bypass = 0;

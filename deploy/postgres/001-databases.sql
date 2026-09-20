-- The local development user owns both databases; production uses separate
-- credentials and grants. Nakama migrations own nakama; fleet-migrate owns fleet.
CREATE DATABASE nakama;
CREATE DATABASE fleet;

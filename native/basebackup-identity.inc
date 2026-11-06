/* Inserted immediately after pg_basebackup establishes its real replication
 * connection, before it creates output directories, slots, or starts a backup.
 * The upstream parser/libpq resolve every original argument and service file.
 * This check does not open a second connection or prompt for a second password.
 */
{
    const char *expected_id = getenv("PGCACHEGUARD_EXPECT_SYSTEM_ID");
    if (expected_id != NULL)
    {
        const char *expected_host = getenv("PGCACHEGUARD_EXPECT_HOST");
        const char *expected_port = getenv("PGCACHEGUARD_EXPECT_PORT");
        char *actual_id = NULL;
        TimeLineID timeline;
        struct sockaddr_storage peer;
        socklen_t peerlen = sizeof(peer);

        if (PQserverVersion(conn) < 180000 || PQserverVersion(conn) >= 190000)
            pg_fatal("pgcacheguard requires a PostgreSQL 18 server");
        if (expected_host == NULL || expected_port == NULL ||
            strcmp(PQhost(conn), expected_host) != 0 ||
            strcmp(PQport(conn), expected_port) != 0 ||
            getpeername(PQsocket(conn), (struct sockaddr *) &peer, &peerlen) != 0 ||
            peer.ss_family != AF_UNIX)
            pg_fatal("pgcacheguard: actual connection is not the configured shared local Unix socket");
        if (!RunIdentifySystem(conn, &actual_id, &timeline, NULL, NULL))
            pg_fatal("pgcacheguard: cannot verify source system identifier");
        if (strcmp(actual_id, expected_id) != 0)
            pg_fatal("pgcacheguard: connected server and mounted PGDATA have different system identifiers");
        pg_free(actual_id);
    }
}

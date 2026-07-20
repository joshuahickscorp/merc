#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

/* Minimal container-local readiness probe, separate from the full Go runtime. */
static int send_all(int fd, const char *buffer, size_t length) {
    size_t sent = 0;
    while (sent < length) {
        ssize_t result = send(fd, buffer + sent, length - sent, 0);
        if (result <= 0) {
            return 0;
        }
        sent += (size_t)result;
    }
    return 1;
}

static ssize_t receive_prefix(int fd, char *buffer, size_t required) {
    size_t received = 0;
    while (received < required) {
        ssize_t result = recv(fd, buffer + received, required - received, 0);
        if (result <= 0) {
            break;
        }
        received += (size_t)result;
    }
    return (ssize_t)received;
}

int main(void) {
    const char *address = getenv("LISTEN_ADDR");
    const char *separator;
    char *end = NULL;
    long port = 8080;
    int fd;
    struct sockaddr_in target;
    struct timeval timeout = {.tv_sec = 3, .tv_usec = 0};
    const char request[] =
        "GET /readyz HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n";
    char response[12] = {0};
    char drain[1024];
    ssize_t received;

    if (address != NULL && (separator = strrchr(address, ':')) != NULL && separator[1] != '\0') {
        errno = 0;
        port = strtol(separator + 1, &end, 10);
        if (errno != 0 || end == separator + 1 || *end != '\0' || port < 1 || port > 65535) {
            return 1;
        }
    }
    fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) {
        return 1;
    }
    if (setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) != 0 ||
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout)) != 0) {
        close(fd);
        return 1;
    }
    memset(&target, 0, sizeof(target));
    target.sin_family = AF_INET;
    target.sin_port = htons((unsigned short)port);
    if (inet_pton(AF_INET, "127.0.0.1", &target.sin_addr) != 1 ||
        connect(fd, (struct sockaddr *)&target, sizeof(target)) != 0) {
        close(fd);
        return 1;
    }
    if (!send_all(fd, request, sizeof(request) - 1)) {
        close(fd);
        return 1;
    }
    received = receive_prefix(fd, response, sizeof(response));
    /*
     * Drain the Connection: close response before releasing the socket. A
     * prefix-only probe can send a TCP reset while the Go server is still
     * writing its readiness JSON; the health check must be observational and
     * must never perturb the process it is checking.
     */
    while (received >= (ssize_t)sizeof(response)) {
        ssize_t drained = recv(fd, drain, sizeof(drain), 0);
        if (drained <= 0) {
            break;
        }
    }
    close(fd);
    if (received < 12) {
        return 1;
    }
    return (memcmp(response, "HTTP/1.0 200", 12) == 0 ||
            memcmp(response, "HTTP/1.1 200", 12) == 0) ? 0 : 1;
}

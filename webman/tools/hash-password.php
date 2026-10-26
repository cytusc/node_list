<?php
// Read from stdin so the plaintext password never enters shell history/process arguments.
$password = rtrim(stream_get_contents(STDIN), "\r\n");
if (strlen($password) < 12) {
    fwrite(STDERR, "请使用至少 12 字节的密码。\n");
    exit(1);
}
echo password_hash($password, PASSWORD_DEFAULT), "\n";

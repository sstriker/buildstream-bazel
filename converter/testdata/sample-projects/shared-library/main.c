/* app: uses the SHARED foo transitively through the STATIC mid */
int mid(void);
int main(void) { return mid() == 43 ? 0 : 1; }

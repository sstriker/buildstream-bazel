int cross_value(void);
int deferred_value(void);
int sub_local(void);
int main(void) { return (cross_value()==42 && deferred_value()==99 && sub_local()==1) ? 0 : 1; }

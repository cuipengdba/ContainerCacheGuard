/* Standalone lab evidence collector. No PostgreSQL paths are built in.
 * create refuses any existing file; drop opens read-only and only advises. */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>
static void fail(const char *what){perror(what);exit(1);}
int main(int argc,char **argv){
 if(argc!=3){fprintf(stderr,"usage: cache-proof create|warm|drop|inspect /private/lab/file\n");return 2;}
 int creating=!strcmp(argv[1],"create");
 int fd=open(argv[2],creating?(O_CREAT|O_EXCL|O_RDWR|O_NOFOLLOW):(O_RDONLY|O_NOFOLLOW),0600);if(fd<0)fail("open");
 if(creating){unsigned char b[65536];for(size_t i=0;i<sizeof(b);i++)b[i]=(unsigned char)(i*37U+13U);for(int n=0;n<4096;n++){ssize_t w=write(fd,b,sizeof(b));if(w!=(ssize_t)sizeof(b))fail("write");}if(fsync(fd))fail("fsync");}
 else if(!strcmp(argv[1],"warm")){unsigned char b[65536];ssize_t n;while((n=read(fd,b,sizeof(b)))>0){}if(n<0)fail("read");}
 else if(!strcmp(argv[1],"drop")){int e=posix_fadvise(fd,0,0,POSIX_FADV_DONTNEED);if(e){errno=e;fail("fadvise");}}
 else if(strcmp(argv[1],"inspect")){fprintf(stderr,"unknown mode\n");return 2;}
 struct stat st;if(fstat(fd,&st))fail("fstat");long page=sysconf(_SC_PAGESIZE);size_t pages=((uint64_t)st.st_size+page-1)/(size_t)page;uint64_t resident=0;
 if(pages){void *m=mmap(NULL,pages*(size_t)page,PROT_NONE,MAP_SHARED,fd,0);if(m==MAP_FAILED)fail("mmap");unsigned char *v=malloc(pages);if(!v)fail("malloc");if(mincore(m,pages*(size_t)page,v))fail("mincore");for(size_t p=0;p<pages;p++)if(v[p]&1)resident+=(uint64_t)page;free(v);munmap(m,pages*(size_t)page);}
 printf("{\"mode\":\"%s\",\"pid\":%ld,\"uid\":%ld,\"device\":%llu,\"inode\":%llu,\"size\":%lld,\"resident_bytes\":%llu,\"mincore_no_data_faults\":true}\n",argv[1],(long)getpid(),(long)getuid(),(unsigned long long)st.st_dev,(unsigned long long)st.st_ino,(long long)st.st_size,(unsigned long long)resident);
 close(fd);return 0;
}

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>
int main(int argc,char **argv){
    if(argc<3)return 2;
    if(!strcmp(argv[1],"create")){
        int fd=open(argv[2],O_CREAT|O_TRUNC|O_RDWR,0600);if(fd<0)return 3;
        char b[65536];for(size_t i=0;i<sizeof(b);i++)b[i]=(char)(i*31+7);
        long mb=argc>3?atol(argv[3]):16;for(long i=0;i<mb*16;i++)if(write(fd,b,sizeof(b))!=sizeof(b))return 4;
        if(fsync(fd))return 5;
        posix_fadvise(fd,0,0,POSIX_FADV_DONTNEED);char one;pread(fd,&one,1,0);close(fd);return 0;
    }
    if(!strcmp(argv[1],"resident")){
        int fd=open(argv[2],O_RDONLY);struct stat st;if(fd<0||fstat(fd,&st))return 3;size_t page=(size_t)sysconf(_SC_PAGESIZE),pages=(st.st_size+page-1)/page;
        void *p=mmap(NULL,st.st_size,PROT_NONE,MAP_SHARED,fd,0);unsigned char *v=calloc(pages,1);if(p==MAP_FAILED||!v||mincore(p,st.st_size,v))return 4;
        size_t n=0;for(size_t i=0;i<pages;i++)n+=v[i]&1;printf("{\"pages\":%zu,\"resident\":%zu,\"first\":%u}\n",pages,n,v[0]&1);munmap(p,st.st_size);free(v);close(fd);return 0;
    }
    if(!strcmp(argv[1],"stdio-copy") && argc==4){
        FILE *in=fopen(argv[2],"rb"),*out=fopen(argv[3],"wbx");if(!in||!out)return 3;
        char b[4096];size_t n;while((n=fread(b,1,sizeof(b),in))>0)if(fwrite(b,1,n,out)!=n)return 4;
        if(ferror(in)||fflush(out)||fsync(fileno(out))||fclose(out)||fclose(in))return 5;
        return 0;
    }
    if(strcmp(argv[1],"copy")||argc<4)return 2;
    int in=open(argv[2],O_RDONLY),out=open(argv[3],O_CREAT|O_EXCL|O_WRONLY,0600);if(in<0||out<0)return 3;
    char b[65536];ssize_t n;while((n=read(in,b,sizeof(b)))>0){if(write(out,b,(size_t)n)!=n){perror("write");return 4;}}
    if(n<0){perror("read");return 5;}if(fsync(out)||close(out)||close(in))return 6;return 0;
}

#!/bin/bash

# SSL证书自动安装脚本
# 用法: ./ssl_install.sh <域名>
# 功能: 自动安装certbot，申请SSL证书，部署到Nginx

set -e  # 遇到错误立即退出

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 检查参数
if [ $# -ne 1 ]; then
    echo -e "${RED}错误: 请指定域名${NC}"
    echo "用法: $0 <域名>"
    echo "示例: $0 example.com"
    exit 1
fi

DOMAIN=$1
NGINX_SSL_DIR="/etc/nginx/ssl/${DOMAIN}"
LETSENCRYPT_DIR="/etc/letsencrypt/live/${DOMAIN}"

echo -e "${GREEN}=== SSL证书自动安装脚本 ===${NC}"
echo -e "域名: ${YELLOW}${DOMAIN}${NC}"
echo ""

# 检查是否为root用户
if [ "$EUID" -ne 0 ]; then 
    echo -e "${RED}错误: 请使用root权限运行此脚本${NC}"
    echo "使用: sudo $0 ${DOMAIN}"
    exit 1
fi

# 函数: 检测CDN
check_cdn() {
    local domain=$1
    echo -e "\n${BLUE}[检测] 检查域名是否使用CDN...${NC}"
    
    # 获取域名的A记录和CNAME记录
    local a_record=$(dig +short A "${domain}" | head -1)
    local cname_record=$(dig +short CNAME "${domain}" | head -1)
    
    echo -e "  A记录: ${YELLOW}${a_record:-未找到}${NC}"
    echo -e "  CNAME记录: ${YELLOW}${cname_record:-未找到}${NC}"
    
    # CDN检测标志
    local cdn_detected=false
    local cdn_type=""
    
    # 检测常见CDN服务商的特征
    # 1. 检查CNAME是否包含CDN特征
    if [[ -n "$cname_record" ]]; then
        case "$cname_record" in
            *cloudflare*|*cdn*|*cloudfront*|*akamai*|*fastly*|*incapsula*)
                cdn_detected=true
                cdn_type="CNAME包含CDN特征: $cname_record"
                ;;
        esac
    fi
    
    # 2. 检查A记录是否属于CDN IP段（通过Whois查询或IP特征）
    if [[ -n "$a_record" ]]; then
        # 查询IP归属（简化版，只检测Cloudflare等常见CDN的ASN）
        local ip_info=$(whois "$a_record" 2>/dev/null | grep -i "org-name:\|orgname:\|descr:" | head -3)
        if echo "$ip_info" | grep -qi "cloudflare\|cloudfront\|akamai\|fastly\|incapsula"; then
            cdn_detected=true
            cdn_type="A记录属于CDN IP段"
        fi
        
        # 检查是否为Cloudflare IP段（常见范围）
        if [[ "$a_record" =~ ^(104\.16\.|104\.17\.|104\.18\.|104\.19\.|104\.20\.|104\.21\.|104\.22\.|104\.23\.|104\.24\.|104\.25\.|104\.26\.|104\.27\.|172\.64\.|172\.65\.|172\.66\.|172\.67\.|188\.114\.|188\.115\.|190\.93\.|191\.96\.|192\.0\.) ]]; then
            cdn_detected=true
            cdn_type="Cloudflare IP段"
        fi
    fi
    
    # 3. 检测HTTP响应头（通过curl探测）
    if command -v curl &> /dev/null; then
        local response_headers=$(curl -s -I "http://${domain}" 2>/dev/null | head -10)
        if echo "$response_headers" | grep -qi "cloudflare\|cf-ray\|x-amz-cf\|x-cache\|akamai\|fastly"; then
            cdn_detected=true
            cdn_type="HTTP响应头包含CDN特征"
        fi
    fi
    
    # 输出检测结果
    if [ "$cdn_detected" = true ]; then
        echo -e "${RED}⚠️  检测到域名可能正在使用CDN服务！${NC}"
        echo -e "${YELLOW}  原因: ${cdn_type}${NC}"
        echo ""
        echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo -e "${YELLOW}⚠️  重要提示：${NC}"
        echo -e "  使用CDN可能会导致证书申请失败（HTTP 521错误）"
        echo ""
        echo -e "${BLUE}  解决方案：${NC}"
        echo -e "  1. 暂时关闭CDN代理（推荐）"
        echo -e "     - 在CDN控制台将域名解析设为'仅DNS'模式"
        echo -e "     - 等待DNS生效（约1-5分钟）"
        echo -e "     - 然后重新运行此脚本"
        echo ""
        echo -e "  2. 使用DNS验证方式"
        echo -e "     - 命令: ${GREEN}sudo certbot certonly --manual --preferred-challenges dns -d ${domain}${NC}"
        echo -e "     - 需要手动添加TXT记录到DNS"
        echo ""
        echo -e "  3. 配置CDN允许访问.well-known目录"
        echo -e "     - 确保CDN回源到Nginx的80端口"
        echo -e "     - 配置Nginx允许访问 /.well-known/ 路径"
        echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo ""
        
        # 询问用户是否继续
        read -p "是否仍要继续尝试申请证书？(可能会失败) [y/N]: " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            echo -e "${RED}已取消证书申请${NC}"
            exit 1
        else
            echo -e "${YELLOW}继续尝试申请证书...${NC}"
        fi
    else
        echo -e "${GREEN}✓ 未检测到明显的CDN服务，可以继续申请${NC}"
    fi
}

# 1. 检查并安装依赖工具
echo -e "${GREEN}[1/6] 检查必要工具...${NC}"
for tool in dig curl whois; do
    if ! command -v $tool &> /dev/null; then
        echo -e "${YELLOW}安装 $tool...${NC}"
        if [ -f /etc/debian_version ]; then
            apt-get install -y dnsutils curl whois
        elif [ -f /etc/redhat-release ]; then
            yum install -y bind-utils curl whois
        fi
        break
    fi
done

# 2. 检测CDN
check_cdn "${DOMAIN}"

# 3. 检查并安装certbot
echo -e "\n${GREEN}[2/6] 检查certbot...${NC}"
if ! command -v certbot &> /dev/null; then
    echo -e "${YELLOW}certbot未安装，开始安装...${NC}"
    
    # 检测操作系统
    if [ -f /etc/debian_version ]; then
        # Debian/Ubuntu
        apt-get update
        apt-get install -y certbot python3-certbot-nginx
    elif [ -f /etc/redhat-release ]; then
        # CentOS/RHEL
        yum install -y epel-release
        yum install -y certbot python3-certbot-nginx
    else
        echo -e "${RED}不支持的操作系统，请手动安装certbot${NC}"
        exit 1
    fi
    
    echo -e "${GREEN}certbot安装完成${NC}"
else
    echo -e "${GREEN}certbot已安装: $(certbot --version)${NC}"
fi

# 4. 检查Nginx状态并停止
echo -e "\n${GREEN}[3/6] 检查Nginx状态...${NC}"
if systemctl is-active --quiet nginx; then
    echo -e "${YELLOW}Nginx正在运行，停止Nginx...${NC}"
    systemctl stop nginx
    sleep 2
    echo -e "${GREEN}Nginx已停止${NC}"
else
    echo -e "${YELLOW}Nginx未运行${NC}"
fi

# 5. 创建证书存放目录
echo -e "\n${GREEN}[4/6] 创建证书目录...${NC}"
mkdir -p "${NGINX_SSL_DIR}"
echo -e "证书目录: ${YELLOW}${NGINX_SSL_DIR}${NC}"

# 6. 申请SSL证书
echo -e "\n${GREEN}[5/6] 申请SSL证书...${NC}"
echo -e "${YELLOW}正在为 ${DOMAIN} 申请证书...${NC}"

# 使用standalone模式申请证书
certbot certonly --standalone \
    -d "${DOMAIN}" \
    --non-interactive \
    --agree-tos \
    --email "admin@${DOMAIN}" \
    --preferred-challenges http \
    --keep-until-expiring

if [ $? -eq 0 ]; then
    echo -e "${GREEN}证书申请成功！${NC}"
else
    echo -e "${RED}证书申请失败${NC}"
    echo -e "${YELLOW}可能的原因：${NC}"
    echo -e "  1. 域名解析未生效或指向错误IP"
    echo -e "  2. CDN服务未正确关闭（请确认已设置为'仅DNS'模式）"
    echo -e "  3. 防火墙阻止了80或443端口访问"
    echo -e "  4. DNS验证记录未正确添加（如果使用了DNS方式）"
    echo ""
    echo -e "${BLUE}建议：${NC}"
    echo -e "  - 检查域名解析: ${GREEN}dig ${DOMAIN}${NC}"
    echo -e "  - 测试HTTP访问: ${GREEN}curl -I http://${DOMAIN}${NC}"
    echo -e "  - 查看详细日志: ${GREEN}certbot certonly --standalone -d ${DOMAIN} -v${NC}"
    exit 1
fi

# 7. 复制证书到Nginx目录
echo -e "\n${GREEN}[6/6] 复制证书到Nginx目录...${NC}"

if [ -d "${LETSENCRYPT_DIR}" ]; then
    # 复制证书文件
    cp -f "${LETSENCRYPT_DIR}/fullchain.pem" "${NGINX_SSL_DIR}/"
    cp -f "${LETSENCRYPT_DIR}/privkey.pem" "${NGINX_SSL_DIR}/"
    
    # 为了兼容性，创建软链接或重命名
    ln -sf "${NGINX_SSL_DIR}/fullchain.pem" "${NGINX_SSL_DIR}/${DOMAIN}.pem"
    ln -sf "${NGINX_SSL_DIR}/privkey.pem" "${NGINX_SSL_DIR}/${DOMAIN}.key"
    
    # 设置权限
    chmod 600 "${NGINX_SSL_DIR}/privkey.pem"
    chmod 644 "${NGINX_SSL_DIR}/fullchain.pem"
    
    echo -e "${GREEN}证书已复制到: ${NGINX_SSL_DIR}${NC}"
    ls -la "${NGINX_SSL_DIR}"
else
    echo -e "${RED}错误: Let's Encrypt证书目录不存在${NC}"
    exit 1
fi

# 启动Nginx
echo -e "\n${GREEN}启动Nginx...${NC}"
systemctl start nginx
if systemctl is-active --quiet nginx; then
    echo -e "${GREEN}Nginx已成功启动${NC}"
else
    echo -e "${RED}Nginx启动失败，请检查配置${NC}"
    systemctl status nginx
    exit 1
fi

# 显示证书信息
echo -e "\n${GREEN}=== 安装完成 ===${NC}"
echo -e "域名: ${YELLOW}${DOMAIN}${NC}"
echo -e "证书路径: ${YELLOW}${NGINX_SSL_DIR}${NC}"
echo -e "证书文件:"
echo -e "  - ${GREEN}fullchain.pem${NC} (证书链)"
echo -e "  - ${GREEN}privkey.pem${NC} (私钥)"
echo -e "  - ${GREEN}${DOMAIN}.pem${NC} (软链接到fullchain.pem)"
echo -e "  - ${GREEN}${DOMAIN}.key${NC} (软链接到privkey.pem)"

# 显示证书有效期
echo -e "\n${GREEN}证书信息:${NC}"
certbot certificates | grep -A 5 "${DOMAIN}" || echo "无法获取证书信息"

echo -e "\n${GREEN}下一步:${NC}"
echo -e "1. 检查Nginx配置: ${YELLOW}nginx -t${NC}"
echo -e "2. 重启Nginx: ${YELLOW}systemctl restart nginx${NC}"
echo -e "3. 访问网站: ${YELLOW}https://${DOMAIN}${NC}"
echo -e "4. 证书自动续期: ${YELLOW}certbot renew --dry-run${NC}"

# 设置自动续期任务
echo -e "\n${GREEN}设置自动续期...${NC}"
if ! crontab -l 2>/dev/null | grep -q "certbot renew"; then
    (crontab -l 2>/dev/null; echo "0 3 * * * /usr/bin/certbot renew --quiet --pre-hook 'systemctl stop nginx' --post-hook 'systemctl start nginx'") | crontab -
    echo -e "${GREEN}已添加自动续期任务 (每天凌晨3点)${NC}"
else
    echo -e "${YELLOW}自动续期任务已存在${NC}"
fi

echo -e "\n${GREEN}脚本执行完成！${NC}"

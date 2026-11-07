import { Button, Result } from 'antd'
import { Link } from 'react-router'

export default function NotFoundPage() {
  return (
    <Result
      status="404"
      title="没有这个页面"
      subTitle="地址可能写错了，或者对象已被删除。"
      extra={
        <Link to="/">
          <Button type="primary">回到运营概览</Button>
        </Link>
      }
    />
  )
}
